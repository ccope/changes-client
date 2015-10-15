package engine

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"

	"github.com/dropbox/changes-client/client"
	"github.com/dropbox/changes-client/client/adapter"
	"github.com/dropbox/changes-client/client/reporter"
	"github.com/dropbox/changes-client/common/sentry"
	"github.com/dropbox/changes-client/common/version"

	_ "github.com/dropbox/changes-client/adapter/basic"
	_ "github.com/dropbox/changes-client/adapter/lxc"
	_ "github.com/dropbox/changes-client/reporter/artifactstore"
	_ "github.com/dropbox/changes-client/reporter/jenkins"
	_ "github.com/dropbox/changes-client/reporter/mesos"
	_ "github.com/dropbox/changes-client/reporter/multireporter"
)

const (
	STATUS_QUEUED      = "queued"
	STATUS_IN_PROGRESS = "in_progress"
	STATUS_FINISHED    = "finished"

	RESULT_PASSED  Result = "passed"
	RESULT_FAILED  Result = "failed"
	RESULT_ABORTED Result = "aborted"
	// Test results unreliable or unavailable due to infrastructure
	// issues.
	RESULT_INFRA_FAILED Result = "infra_failed"

	SNAPSHOT_ACTIVE = "active"
	SNAPSHOT_FAILED = "failed"
)

type Result string

func (r Result) String() string {
	return string(r)
}

// Convenience method to check for all types of failure.
func (r Result) IsFailure() bool {
	switch r {
	case RESULT_FAILED, RESULT_INFRA_FAILED:
		return true
	}
	return false
}

var (
	selectedAdapterFlag  string
	selectedReporterFlag string
	outputSnapshotFlag   string
)

type Engine struct {
	config    *client.Config
	clientLog *client.Log
	adapter   adapter.Adapter
	reporter  reporter.Reporter
}

func RunBuildPlan(config *client.Config) (Result, error) {
	forceInfraFailure := false
	if config.GetDebugConfig("forceInfraFailure", &forceInfraFailure); forceInfraFailure {
		return RESULT_INFRA_FAILED,
			errors.New("Infra failure forced for debugging")
	}

	currentAdapter, err := adapter.Create(selectedAdapterFlag)
	if err != nil {
		return RESULT_INFRA_FAILED, err
	}

	currentReporter, err := reporter.Create(selectedReporterFlag)
	if err != nil {
		log.Printf("[engine] failed to initialize reporter: %s", selectedReporterFlag)
		return RESULT_INFRA_FAILED, err
	}
	log.Printf("[engine] started with reporter %s, adapter %s", selectedReporterFlag, selectedAdapterFlag)

	engine := &Engine{
		config:    config,
		clientLog: client.NewLog(),
		adapter:   currentAdapter,
		reporter:  currentReporter,
	}

	return engine.Run()
}

// Returns the ID to use for the generated snapshot, or an empty string if no
// snapshot should be generated. Use this instead of the flag or config value.
func (e *Engine) outputSnapshotID() string {
	// Until we're confident that the config always matches the flag, use the
	// flag to preserve behavior.
	return outputSnapshotFlag
}

// checkForSnapshotInconsistency returns an error if the output snapshot specified via flag
// appears inconsistent with the value from the JobStep config.
func (e *Engine) checkForSnapshotInconsistency() error {
	if outputSnapshotFlag != e.config.ExpectedSnapshot.ID {
		return fmt.Errorf("Output snapshot mismatch; flag was %q, but config was %q",
			outputSnapshotFlag, e.config.ExpectedSnapshot.ID)
	}
	return nil
}

func (e *Engine) Run() (Result, error) {
	e.reporter.Init(e.config)
	defer e.reporter.Shutdown()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		reportLogChunks("console", e.clientLog, e.reporter)
		wg.Done()
	}()

	e.clientLog.Writeln("changes-client version: " + version.GetVersion())
	e.clientLog.Printf("Running jobstep %s for %s (%s)", e.config.JobstepID, e.config.Project.Name, e.config.Project.Slug)

	if err := e.checkForSnapshotInconsistency(); err != nil {
		sentry.Error(err, map[string]string{})
		// Ugly, but better to be consistent.
		// TODO(kylec): Remove this once we're confident in the config value.
		e.config.ExpectedSnapshot.ID = e.outputSnapshotID()
	}

	e.reporter.PushJobstepStatus(STATUS_IN_PROGRESS, "")

	result, err := e.runBuildPlan()

	e.clientLog.Printf("==> Build finished! Recorded result as %s", result)

	e.reporter.PushJobstepStatus(STATUS_FINISHED, result.String())

	e.clientLog.Close()

	wg.Wait()

	return result, err
}

func (e *Engine) executeCommands() (Result, error) {
	for _, cmdConfig := range e.config.Cmds {
		e.clientLog.Printf("==> Running command %s", cmdConfig.ID)
		e.clientLog.Printf("==>     with script %s", cmdConfig.Script)
		cmd, err := client.NewCommand(cmdConfig.ID, cmdConfig.Script)
		if err != nil {
			e.reporter.PushCommandStatus(cmd.ID, STATUS_FINISHED, 255)
			e.clientLog.Printf("==> Error creating command script: %s", err)
			return RESULT_INFRA_FAILED, err
		}
		e.reporter.PushCommandStatus(cmd.ID, STATUS_IN_PROGRESS, -1)

		cmd.CaptureOutput = cmdConfig.CaptureOutput

		env := os.Environ()
		for k, v := range cmdConfig.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env

		if len(cmdConfig.Cwd) > 0 {
			cmd.Cwd = cmdConfig.Cwd
		}

		cmdResult, err := e.adapter.Run(cmd, e.clientLog)

		if err != nil {
			e.reporter.PushCommandStatus(cmd.ID, STATUS_FINISHED, 255)
			e.clientLog.Printf("==> Error running command: %s", err)
			return RESULT_INFRA_FAILED, err
		}
		result := RESULT_FAILED
		if cmdResult.Success {
			result = RESULT_PASSED
			if cmd.CaptureOutput {
				e.reporter.PushCommandOutput(cmd.ID, STATUS_FINISHED, 0, cmdResult.Output)
			} else {
				e.reporter.PushCommandStatus(cmd.ID, STATUS_FINISHED, 0)
			}
		} else {
			e.reporter.PushCommandStatus(cmd.ID, STATUS_FINISHED, 1)
		}

		if err := e.reporter.PublishArtifacts(cmdConfig, e.adapter, e.clientLog); err != nil {
			e.clientLog.Printf("==> PublishArtifacts Error: %s", err)
			return RESULT_INFRA_FAILED, err
		}

		if result.IsFailure() {
			return result, nil
		}
	}

	// Made it through all commands without failure. Success.
	return RESULT_PASSED, nil
}

func (e *Engine) captureSnapshot() error {
	log.Printf("[adapter] Capturing snapshot %s", e.outputSnapshotID())
	err := e.adapter.CaptureSnapshot(e.outputSnapshotID(), e.clientLog)
	if err != nil {
		log.Printf("[adapter] Failed to capture snapshot: %s", err)
		return err
	}
	return nil
}

func (e *Engine) runBuildPlan() (Result, error) {
	// cancellation signal
	cancel := make(chan struct{})

	// capture ctrl+c and enforce a clean shutdown
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt)
	go func() {
		shuttingDown := false
		for _ = range sigchan {
			if shuttingDown {
				log.Printf("Second interrupt received. Terminating!")
				os.Exit(1)
			}

			shuttingDown = true

			log.Printf("Interrupted! Cancelling execution and cleaning up..")
			cancel <- struct{}{}
		}
	}()

	// We need to ensure that we're able to abort the build if upstream suggests
	// that it's been cancelled.
	if !e.config.Debug {
		go func() {
			um := &UpstreamMonitor{
				Config: e.config,
			}
			um.WaitUntilAbort()
			cancel <- struct{}{}
		}()
	}

	if err := e.adapter.Init(e.config); err != nil {
		log.Print(fmt.Sprintf("[adapter] %s", err))
		e.clientLog.Printf("==> ERROR: Failed to initialize %s adapter", selectedAdapterFlag)
		return RESULT_INFRA_FAILED, err
	}

	metrics, err := e.adapter.Prepare(e.clientLog)
	if err != nil {
		log.Printf("[adapter] %s", err)
		e.clientLog.Printf("==> ERROR: %s adapter failed to prepare: %s", selectedAdapterFlag, err)
		return RESULT_INFRA_FAILED, err
	}
	defer e.adapter.Shutdown(e.clientLog)
	e.reporter.ReportMetrics(metrics)

	type cmdResult struct {
		result Result
		err    error
	}
	// actually begin executing the build plan
	finished := make(chan cmdResult)
	go func() {
		r, cmderr := e.executeCommands()
		finished <- cmdResult{r, cmderr}
	}()

	var result Result
	select {
	case cmdresult := <-finished:
		if cmdresult.err != nil {
			return cmdresult.result, cmdresult.err
		}
		result = cmdresult.result
	case <-cancel:
		e.clientLog.Writeln("==> ERROR: Build was aborted by upstream")
		return RESULT_ABORTED, nil
	}

	if result == RESULT_PASSED && e.outputSnapshotID() != "" {
		var snapshotStatus string
		sserr := e.captureSnapshot()
		if sserr != nil {
			snapshotStatus = SNAPSHOT_FAILED
		} else {
			snapshotStatus = SNAPSHOT_ACTIVE
		}
		e.reporter.PushSnapshotImageStatus(e.outputSnapshotID(), snapshotStatus)
		if sserr != nil {
			return RESULT_INFRA_FAILED, sserr
		}
	}
	return result, nil
}

func reportLogChunks(name string, clientLog *client.Log, r reporter.Reporter) {
	for ch, ok := clientLog.GetChunk(); ok; ch, ok = clientLog.GetChunk() {
		r.PushLogChunk(name, ch)
	}
}

func init() {
	flag.StringVar(&selectedAdapterFlag, "adapter", "basic", "Adapter to run build against")
	flag.StringVar(&selectedReporterFlag, "reporter", "multireporter", "Reporter to send results to")
	flag.StringVar(&outputSnapshotFlag, "save-snapshot", "", "Save the resulting container snapshot")
}
