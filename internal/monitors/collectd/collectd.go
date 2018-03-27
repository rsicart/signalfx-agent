package collectd

//go:generate collectd-template-to-go collectd.conf.tmpl collectd.conf.go

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/event"
	"github.com/signalfx/signalfx-agent/internal/core/config"
	"github.com/signalfx/signalfx-agent/internal/monitors/types"
	"github.com/signalfx/signalfx-agent/internal/utils"
)

const (
	// How long to wait for back-to-back (re)starts before actually (re)starting
	restartDelay = 3 * time.Second
)

// Collectd states
const (
	Errored       = "errored"
	Restarting    = "restarting"
	Running       = "running"
	Starting      = "starting"
	Stopped       = "stopped"
	ShuttingDown  = "shutting-down"
	Uninitialized = "uninitialized"
)

// Manager coordinates the collectd conf file and running the embedded collectd
// library.
type Manager struct {
	configMutex sync.Mutex
	conf        *config.CollectdConfig
	// Map of each active monitor to its output instance
	activeMonitors  map[types.MonitorID]types.Output
	genericJMXUsers map[types.MonitorID]bool
	active          bool
	// The port of the active write server, will be 0 if write server isn't
	// started yet.
	writeServerPort int

	// Channels to control the state machine asynchronously
	stop chan struct{}
	// Closed when collectd state machine is terminated
	terminated     chan struct{}
	requestRestart chan struct{}

	logger *log.Entry
}

var collectdSingleton *Manager

// MainInstance returns the global singleton instance of the collectd manager
func MainInstance() *Manager {
	if collectdSingleton == nil {
		panic("Main collectd instance should not be accessed before being configured")
	}

	return collectdSingleton
}

// InitCollectd makes a new instance of a manager and initializes it, but does
// not start collectd
func InitCollectd(conf *config.CollectdConfig) *Manager {
	manager := &Manager{
		conf:            conf,
		activeMonitors:  make(map[types.MonitorID]types.Output),
		genericJMXUsers: make(map[types.MonitorID]bool),
		requestRestart:  make(chan struct{}),
		logger:          log.WithField("collectdInstance", conf.InstanceName),
	}
	manager.deleteExistingConfig()

	return manager
}

// ConfigureMainCollectd should be called whenever the main collectd config in
// the agent has changed.  Restarts collectd if the config has changed.
func ConfigureMainCollectd(conf *config.CollectdConfig) error {
	localConf := *conf
	localConf.InstanceName = "global"

	if collectdSingleton == nil {
		collectdSingleton = InitCollectd(&localConf)
	}
	cm := collectdSingleton

	cm.configMutex.Lock()
	defer cm.configMutex.Unlock()

	if cm.conf == nil || cm.conf.Hash() != localConf.Hash() {
		cm.conf = &localConf

		cm.RequestRestart()
	}

	return nil
}

// ConfigureFromMonitor is how monitors notify the collectd manager that they
// have added a configuration file to managed_config and need a restart. The
// monitorID is passed in so that we can keep track of what monitors are
// actively using collectd.  When a monitor is done (i.e. shutdown) it should
// call MonitorDidShutdown.  GenericJMX monitors should set usesGenericJMX to
// true so that collectd can know to load the java plugin in the collectd.conf
// file so that any JVM config doesn't get set multiple times and cause
// spurious log output.
func (cm *Manager) ConfigureFromMonitor(monitorID types.MonitorID, output types.Output, usesGenericJMX bool) error {
	cm.configMutex.Lock()
	defer cm.configMutex.Unlock()

	cm.activeMonitors[monitorID] = output

	// This is kind of ugly having to keep track of this but it allows us to
	// load the GenericJMX plugin in a central place and then have each
	// GenericJMX monitor render its own config file and not have to worry
	// about reinitializing GenericJMX and causing errors to be thrown.
	if usesGenericJMX {
		cm.genericJMXUsers[monitorID] = true
	}

	cm.RequestRestart()
	return nil
}

// MonitorDidShutdown should be called by any monitor that uses collectd when
// it is shutdown.
func (cm *Manager) MonitorDidShutdown(monitorID types.MonitorID) {
	cm.configMutex.Lock()
	defer cm.configMutex.Unlock()

	delete(cm.genericJMXUsers, monitorID)

	// This is a bit hacky but it is to solve the race condition where the
	// monitor "shuts down" in the agent but is still running in collectd and
	// generating datapoints.  If datapoints come in after the monitor's output
	// is lost from activeMonitors, then those datapoints can't be associated
	// with an Output interface and will be dropped, causing scary looking
	// error messages. Blocking the monitor's Shutdown method until collectd is
	// restarted is undesirable since it is called synchronously.  This defers
	// the actual deletion of the Output interfaces until collectd has been
	// restarted and is no longer sending datapoints for the deleted monitor.
	go func() {
		time.Sleep(5 * time.Second)

		cm.configMutex.Lock()
		defer cm.configMutex.Unlock()

		delete(cm.activeMonitors, monitorID)

		if len(cm.activeMonitors) == 0 && !utils.IsSignalChanClosed(cm.stop) {
			close(cm.stop)
			cm.deleteExistingConfig()
			<-cm.terminated
		}
	}()

	if len(cm.activeMonitors) > 1 {
		cm.RequestRestart()
	}
}

// RequestRestart should be used to indicate that a configuration in
// managed_config has been updated (e.g. by a monitor) and that collectd needs
// to restart.  This method will not immediately restart but will wait for a
// bit to batch together multiple back-to-back restarts.
func (cm *Manager) RequestRestart() {
	if cm.terminated == nil || utils.IsSignalChanClosed(cm.terminated) {
		waitCh := make(chan struct{})
		cm.terminated = make(chan struct{})
		// This should only have to be called once for the lifetime of the
		// agent.
		go cm.manageCollectd(waitCh, cm.terminated)
		// Wait for the write server to be started
		<-waitCh
	}

	cm.requestRestart <- struct{}{}
}

// WriteServerURL returns the URL of the write server, in case monitors need to
// know it (e.g. the signalfx-metadata plugin).
func (cm *Manager) WriteServerURL() string {
	// Just reuse the config struct's method for making a URL
	conf := *cm.conf
	conf.WriteServerPort = uint16(cm.writeServerPort)
	return conf.WriteServerURL()
}

// Config returns the collectd config used by this instance of collectd manager
func (cm *Manager) Config() *config.CollectdConfig {
	if cm.conf == nil {
		// This is a programming bug if we get here.
		panic("Collectd must be configured before any monitor tries to use it")
	}
	return cm.conf
}

// ManagedConfigDir returns the directory where monitor config should go.
func (cm *Manager) ManagedConfigDir() string {
	if cm.conf == nil {
		// This is a programming bug if we get here.
		panic("Collectd must be configured before any monitor tries to use it")
	}
	return cm.conf.ManagedConfigDir()
}

// PluginDir returns the base directory that holds both C and Python plugins.
func (cm *Manager) PluginDir() string {
	if cm.conf == nil {
		// This is a programming bug if we get here.
		panic("Collectd must be configured before any monitor tries to use it")
	}
	return filepath.Join(cm.conf.BundleDir, "plugins/collectd")
}

// Manage the subprocess with a basic state machine.  This is a bit tricky
// since we have config coming in asynchronously from multiple sources.  This
// function should never return.  waitCh will be closed once the write server
// is setup and right before it is actualy waiting for restart signals.
func (cm *Manager) manageCollectd(initCh chan<- struct{}, terminated chan struct{}) {
	state := Uninitialized
	// The collectd process manager
	var cmd *exec.Cmd
	// Where collectd's output goes
	var output io.ReadCloser
	procDied := make(chan struct{})
	restart := make(chan struct{})
	var restartDebounced func()
	var restartDebouncedStop chan<- struct{}

	cm.stop = make(chan struct{})

	writeServer, err := cm.startWriteServer()
	if err != nil {
		cm.logger.WithError(err).Error("Could not start collectd write server")
		state = Errored
	} else {
		cm.writeServerPort = writeServer.RunningPort()
	}

	cm.setCollectdVersionEnvVar()

	close(initCh)

	for {
		cm.logger.Debugf("Collectd is now %s", state)

		switch state {

		case Uninitialized:
			restartDebounced, restartDebouncedStop = utils.Debounce0(func() {
				restart <- struct{}{}
			}, restartDelay)

			go func() {
				for {
					select {
					case <-cm.requestRestart:
						restartDebounced()
					case <-terminated:
						return
					}
				}
			}()

			// Block here until we actually get a start or stop request
			select {
			case <-cm.stop:
				state = Stopped
			case <-restart:
				state = Starting
			}

		case Starting:
			if err := cm.rerenderConf(writeServer.RunningPort()); err != nil {
				cm.logger.WithError(err).Error("Could not render collectd.conf")
				state = Stopped
				continue
			}

			cmd, output = cm.makeChildCommand()

			if err := cmd.Start(); err != nil {
				cm.logger.WithError(err).Error("Could not start collectd child process!")
				time.Sleep(restartDelay)
				state = Starting
				continue
			}

			go func() {
				scanner := logScanner(output)
				for scanner.Scan() {
					logLine(scanner.Text(), cm.logger)
				}
			}()

			go func() {
				cmd.Wait()
				output.Close()
				procDied <- struct{}{}
			}()

			state = Running

		case Running:
			select {
			case <-restart:
				state = Restarting
			case <-cm.stop:
				state = ShuttingDown
			case <-procDied:
				cm.logger.Error("Collectd died when it was supposed to be running, restarting...")
				time.Sleep(restartDelay)
				state = Starting
			}

		case Restarting:
			cmd.Process.Kill()
			<-procDied
			state = Starting

		case ShuttingDown:
			cmd.Process.Kill()
			<-procDied
			state = Stopped

		case Stopped:
			restartDebouncedStop <- struct{}{}
			writeServer.Shutdown()
			close(terminated)
			return
		}
	}
}

// Delete existing config in case there were plugins configured before that won't
// be configured on this run.
func (cm *Manager) deleteExistingConfig() {
	if cm.conf != nil {
		cm.logger.Debugf("Deleting existing config from %s", cm.conf.InstanceConfigDir())
		os.RemoveAll(cm.conf.InstanceConfigDir())
	}
}

func (cm *Manager) startWriteServer() (*WriteHTTPServer, error) {
	writeServer, err := NewWriteHTTPServer(cm.conf.WriteServerIPAddr, cm.conf.WriteServerPort, cm.receiveDPs, cm.receiveEvents)
	if err != nil {
		return nil, err
	}

	if err := writeServer.Start(); err != nil {
		return nil, err
	}

	cm.logger.WithFields(log.Fields{
		"ipAddr": cm.conf.WriteServerIPAddr,
		"port":   writeServer.RunningPort(),
	}).Info("Started collectd write server")

	return writeServer, nil
}

func (cm *Manager) receiveDPs(dps []*datapoint.Datapoint) {
	cm.configMutex.Lock()
	defer cm.configMutex.Unlock()

	for i := range dps {
		var monitorID types.MonitorID
		if id, ok := dps[i].Meta["monitorID"].(string); ok {
			monitorID = types.MonitorID(id)
		} else if id := dps[i].Dimensions["monitorID"]; id != "" {
			monitorID = types.MonitorID(id)
			delete(dps[i].Dimensions, "monitorID")
		}

		var output types.Output
		if string(monitorID) == "" {
			cm.logger.WithFields(log.Fields{
				"monitorID": monitorID,
				"datapoint": dps[i],
			}).Error("Datapoint does not specify its monitor id, cannot process")
			continue
		} else {
			output = cm.activeMonitors[monitorID]
		}

		if output == nil {
			if output == nil {
				cm.logger.WithFields(log.Fields{
					"monitorID": monitorID,
					"datapoint": dps[i],
				}).Error("Datapoint has an unknown monitorID")
				continue
			}
		}

		output.SendDatapoint(dps[i])
	}
}

func (cm *Manager) receiveEvents(events []*event.Event) {
	cm.configMutex.Lock()
	defer cm.configMutex.Unlock()

	for i := range events {
		var monitorID types.MonitorID
		if id, ok := events[i].Properties["monitorID"].(string); ok {
			monitorID = types.MonitorID(id)
			delete(events[i].Properties, "monitorID")
		} else if id := events[i].Dimensions["monitorID"]; id != "" {
			monitorID = types.MonitorID(id)
			delete(events[i].Dimensions, "monitorID")
		}

		if string(monitorID) == "" {
			cm.logger.WithFields(log.Fields{
				"event": spew.Sdump(events[i]),
			}).Error("Event does not have a monitorID as either a dimension or property field, cannot send")
			continue
		}

		output := cm.activeMonitors[monitorID]
		if output == nil {
			cm.logger.WithFields(log.Fields{
				"monitorID": monitorID,
			}).Error("Event has an unknown monitorID, cannot send")
			continue
		}

		output.SendEvent(events[i])
	}
}

func (cm *Manager) rerenderConf(writeHTTPPort int) error {
	output := bytes.Buffer{}

	cm.logger.WithFields(log.Fields{
		"context": cm.conf,
	}).Debug("Rendering main collectd.conf template")

	// Copy so that hash of config struct is consistent
	conf := *cm.conf
	conf.HasGenericJMXMonitor = len(cm.genericJMXUsers) > 0
	conf.WriteServerPort = uint16(writeHTTPPort)

	if err := CollectdTemplate.Execute(&output, &conf); err != nil {
		return errors.Wrapf(err, "Failed to render collectd template")
	}

	return WriteConfFile(output.String(), cm.conf.ConfigFilePath())
}

func (cm *Manager) makeChildCommand() (*exec.Cmd, io.ReadCloser) {
	loader := filepath.Join(cm.conf.BundleDir, "lib64/ld-linux-x86-64.so.2")
	collectdBin := filepath.Join(cm.conf.BundleDir, "bin/collectd")
	args := []string{"-f", "-C", cm.conf.ConfigFilePath()}

	var cmd *exec.Cmd
	// If running in a container where the bundle is the main filesystem, don't
	// bother explicitly invoking through the loader (this happens
	// automatically).
	if cm.conf.BundleDir == "/" {
		cmd = exec.Command(collectdBin, args...)
	} else {
		cmd = exec.Command(loader, append([]string{collectdBin}, args...)...)
	}

	// Send both stdout and stderr to the same buffer
	r, w, err := os.Pipe()
	// If this errors things are really wrong with the system
	if err != nil {
		panic("Output pipe could not be created for collectd")
	}
	cmd.Stdout = w
	cmd.Stderr = w

	// This is Linux-specific and will cause collectd to be killed by the OS if
	// the agent dies
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
	}

	return cmd, r
}

var collectdVersionRegexp = regexp.MustCompile(`collectd (?P<version>.*), http://collectd.org/`)

func (cm *Manager) setCollectdVersionEnvVar() {
	loader := filepath.Join(cm.conf.BundleDir, "lib64/ld-linux-x86-64.so.2")
	collectdBin := filepath.Join(cm.conf.BundleDir, "bin/collectd")
	cmd := exec.Command(loader, collectdBin, "-h")

	outText, err := cmd.CombinedOutput()
	if err != nil {
		cm.logger.WithError(err).Error("Could not determine collectd version")
		return
	}

	groups := utils.RegexpGroupMap(collectdVersionRegexp, string(outText))
	if groups == nil {
		cm.logger.Errorf("Could not determine collectd version from output %s", outText)
		return
	}

	os.Setenv("COLLECTD_VERSION", groups["version"])
	cm.logger.Infof("Detected collectd version %s", groups["version"])
}
