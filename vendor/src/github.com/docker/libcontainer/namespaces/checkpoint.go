package namespaces

import (
	"fmt"
	"os/exec"
	"strconv"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/utils"
)

const (
	CriuCmd       = "criu"
	CheckpointLog = "dump.log"
	RestoreLog    = "restore.log"
	PidFile       = "restore.pid"
)

// Checkpoint the specified container using the criu(8) utility.
func Checkpoint(container *libcontainer.Config, imageDir string, initPid int) error {
	var (
		criuCmdPath string
		err         error
	)

	if criuCmdPath, err = utils.WhichPath(CriuCmd); err != nil {
		return err
	}

	// Prepare basic command line arguments.
	args := []string{
		"dump", "-v4",
		"-D", imageDir, "-o", CheckpointLog,
		"--root", container.RootFs,
		"--manage-cgroups", "--evasive-devices", "--tcp-established",
		"-t", strconv.Itoa(initPid),
	}

	// Append arguments for external bind mounts.
	for _, mountpoint := range container.MountConfig.Mounts {
		args = append(args, "--ext-mount-map", fmt.Sprintf("%s:%s", mountpoint.Destination, mountpoint.Destination))
	}

	// Execute criu to checkpoint.
	log.Debugf("Running CRIU: %s %v", criuCmdPath, args)
	output, err := exec.Command(criuCmdPath, args...).CombinedOutput()
	if len(output) > 0 {
		log.Debugf("%s", output)
	}
	return err
}
