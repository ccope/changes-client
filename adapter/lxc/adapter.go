// +build linux lxc

package lxcadapter

import (
	"flag"
	"github.com/dropbox/changes-client/client"
	"github.com/dropbox/changes-client/client/adapter"
	"github.com/dropbox/changes-client/common/glob"
	"log"
	"path"
	"path/filepath"
	"strings"
)

var (
	preLaunch     string
	postLaunch    string
	s3Bucket      string
	release       string
	arch          string
	dist          string
	keepContainer bool
	memory        int
	cpus          int
	backendstr    string
)

type Adapter struct {
	config    *client.Config
	container *Container
	workspace string
}

func (a *Adapter) Init(config *client.Config) error {
	var snapshot string = config.Snapshot.ID
	if snapshot != "" {
		if s3Bucket == "" {
			log.Print("[lxc] WARNING: s3bucket is not defined, snapshot ignored")
			snapshot = ""
		} else {
			snapshot = adapter.FormatUUID(snapshot)
		}
	}

	container := &Container{
		Name:       config.JobstepID,
		Arch:       arch,
		Dist:       dist,
		Release:    release,
		PreLaunch:  preLaunch,
		PostLaunch: postLaunch,
		Snapshot:   snapshot,
		// TODO(dcramer):  Move S3 logic into core engine
		S3Bucket:    s3Bucket,
		MemoryLimit: memory,
		CpuLimit:    cpus,
                BackendStr:  backendstr,
	}

	a.config = config
	a.container = container

	return nil
}

// Prepare the environment for future commands. This is run before any
// commands are processed and is run once.
func (a *Adapter) Prepare(clientLog *client.Log) error {
	err := a.container.Launch(clientLog)
	if err != nil {
		return err
	}

	workspace := "/home/ubuntu"
	if a.config.Workspace != "" {
		workspace = path.Join(workspace, a.config.Workspace)
	}
	workspace, err = filepath.Abs(path.Join(a.container.RootFs(), strings.TrimLeft(workspace, "/")))
	if err != nil {
		return err
	}
	a.workspace = workspace

	return nil
}

// Runs a given command. This may be called multiple times depending
func (a *Adapter) Run(cmd *client.Command, clientLog *client.Log) (*client.CommandResult, error) {
	return a.container.RunCommandInContainer(cmd, clientLog, "ubuntu")
}

// Perform any cleanup actions within the environment.
func (a *Adapter) Shutdown(clientLog *client.Log) error {
	if keepContainer {
		return nil
	}
	return a.container.Destroy()
}

func (a *Adapter) CaptureSnapshot(outputSnapshot string, clientLog *client.Log) error {
	outputSnapshot = adapter.FormatUUID(outputSnapshot)

	err := a.container.CreateImage(outputSnapshot, clientLog)
	if err != nil {
		return err
	}

	if a.container.S3Bucket != "" {
		err = a.container.UploadImage(outputSnapshot, clientLog)
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) GetRootFs() string {
	return a.container.RootFs()
}

func (a *Adapter) CollectArtifacts(artifacts []string, clientLog *client.Log) ([]string, error) {
	log.Printf("[lxc] Searching for %s in %s", artifacts, a.workspace)
	return glob.GlobTree(a.workspace, artifacts)
}

func init() {
	flag.StringVar(&preLaunch, "pre-launch", "", "Container pre-launch script")
	flag.StringVar(&postLaunch, "post-launch", "", "Container post-launch script")
	flag.StringVar(&s3Bucket, "s3-bucket", "", "S3 bucket name")
	flag.StringVar(&dist, "dist", "ubuntu", "Linux distribution")
	flag.StringVar(&release, "release", "trusty", "Distribution release")
	flag.StringVar(&arch, "arch", "amd64", "Linux architecture")
	flag.IntVar(&memory, "memory", 0, "Memory limit")
	flag.IntVar(&cpus, "cpus", 0, "CPU limit")
	flag.BoolVar(&keepContainer, "keep-container", false, "Do not destroy the container on cleanup")
        flag.StringVar(&backendstr, "backend", "lvm", "Backend to use for LXC (dir/lvm/etc)")

	adapter.Register("lxc", &Adapter{})
}
