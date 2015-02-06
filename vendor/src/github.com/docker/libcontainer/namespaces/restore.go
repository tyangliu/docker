package namespaces

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/networkdriver/bridge"
	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/system"
	"github.com/docker/libcontainer/utils"
)

type RestoreCommand func(childPipe *os.File, args []string) *exec.Cmd

type syncMsg struct {
	ExecCriu int `json:"exec_criu"`
}

// Restore the specified container (previously checkpointed) using the
// criu(8) utility.
func Restore(container *libcontainer.Config, stdin io.Reader, stdout, stderr io.Writer, console, dataPath, criuImageDir string, restoreCommand RestoreCommand, restoreCallback func(int) error) (int, error) {
	var (
		criuCmdPath string
		err         error
	)

	if criuCmdPath, err = utils.WhichPath(CriuCmd); err != nil {
		return -1, err
	}

	// Create a pipe to syncronize with the child process
	// that will exec CRIU.  We need this synchronization
	// to save the container's standard file descriptor names
	// before they're potentially closed or moved.
	parent, child, err := newInitPipe()
	if err != nil {
		return -1, err
	}
	defer parent.Close()

	// Build CRIU's argument list.
	pidFile := filepath.Join(criuImageDir, PidFile)
	args, err := buildCriuArgs(container, criuImageDir, pidFile, criuCmdPath)
	if args == nil {
		return -1, err
	}

	// Execute CRIU to restore.
	// Note that this doesn't directly execute CRIU.
	// It executes dockerinit which will exec CRIU
	// after syncing with us (see below).
	command := restoreCommand(child, args)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	log.Debugf("Running %s to execute CRIU:\n%s %v", command.Path, criuCmdPath, args)
	err = command.Start()
	if err != nil {
		log.Debugf("%s", err)
		child.Close()
		return -1, err
	}
	child.Close()

	terminate := func(terr error) (int, error) {
		command.Process.Kill()
		return -1, terr
	}

	// Save standard descriptor names before the child
	// potentially does anything to them (e.g., close(), dup2()).
	if err = saveStdPipes(command.Process.Pid, &container.StdFds); err != nil {
		terminate(err)
	}

	// Now that we have saved the standard descriptor names,
	// tell the child it can proceed.
	s := syncMsg{1}
	if err := json.NewEncoder(parent).Encode(s); err != nil {
		log.Debugf("Cannot sync with child: %s", err)
		terminate(err)
	}

	// XXX Use command.Process.Wait() here instead of command.Wait()
	//     because for some reason command.Wait() hangs!
	if st, err := command.Process.Wait(); err != nil {
		log.Debugf("%s", err)
		terminate(err)
	} else if !st.Success() {
		terminate(fmt.Errorf("%s did not exit successfully\n", criuCmdPath))
	}

	// Read the pid of the restored process, update state.json,
	// and write it out.
	var restorePid int
	if restorePid, err = getRestorePid(pidFile); err != nil {
		terminate(err)
	}
	if err = updateState(restorePid, criuImageDir, dataPath); err != nil {
		terminate(err)
	}
	defer libcontainer.DeleteState(dataPath)

	// Call our callback to signal that we're done.
	if restoreCallback != nil {
		restoreCallback(restorePid)
	}

	// Wait for the container to exit.
	var (
		wstatus syscall.WaitStatus
		rusage  syscall.Rusage
	)
	if _, err = syscall.Wait4(restorePid, &wstatus, 0, &rusage); err != nil {
		return 0, err
	}
	return wstatus.ExitStatus(), err
}

// Return the complete argument list for restoring the specified
// container from its checkpointed image.
func buildCriuArgs(container *libcontainer.Config, criuImageDir, pidFile, criuCmdPath string) ([]string, error) {
	// Make sure pidFile doesn't exist (from a previous restore).
	err := os.Remove(pidFile)
	if err != nil && !os.IsNotExist(err) {
		log.Debugf("Cannot remove %s: %s", pidFile, err)
		return nil, err
	}

	// Prepare basic command line arguments.
	args := []string{
		criuCmdPath, "restore", "-d", "-v4",
		"-D", criuImageDir, "-o", RestoreLog, "--pidfile", pidFile,
		"--root", container.RootFs,
		"--manage-cgroups", "--evasive-devices", "--tcp-established",
	}

	// Append arguments for external bind mounts.
	for _, mountpoint := range container.MountConfig.Mounts {
		args = append(args, "--ext-mount-map", fmt.Sprintf("%s:%s", mountpoint.Destination, mountpoint.Source))
	}

	// Append arguments for the veth device.
	//
	// If the container had a veth network device, generate a random name
	// for the peer veth device in the global namespace and tell CRIU to move
	// it to the Docker's bridge.
	//
	// XXX Note that we assume the contianer has not changed its network
	//     interface name from eth0 to something else.  We should handle
	//     the case where the name has changed by examining /proc/<pid>/net/dev.
	for _, n := range container.Networks {
		if n.Type == "veth" {
			var peerName string
			if peerName, err = utils.GenerateRandomName(n.VethPrefix, 7); err != nil {
				return nil, err
			}
			args = append(args, "--veth-pair", fmt.Sprintf("eth0=%s@%s", peerName, bridge.BridgeName()))
			break
		}
	}

	// Append arguments for external pipes.
	//
	// Pipes that were previously set up for std{in,out,err}
	// were removed after checkpoint.  Use the new ones passed to us.
	for i := 0; i < 3; i++ {
		if s := container.StdFds[i]; strings.Contains(s, "pipe:") {
			args = append(args, "--inherit-fd", fmt.Sprintf("fd[%d]:%s", i, s))
		}
	}

	return args, nil
}

// Save process's std{in,out,err} file names as these will be
// removed if/when the container is checkpointed.  We will need
// this info to restore the container.
func saveStdPipes(pid int, stdFds *[3]string) error {
	dirPath := filepath.Join("/proc", strconv.Itoa(pid), "/fd")
	for i := 0; i < 3; i++ {
		f := filepath.Join(dirPath, strconv.Itoa(i))
		target, err := os.Readlink(f)
		if err != nil {
			return err
		}
		stdFds[i] = target
	}
	return nil
}

// Return the PID of the restored process that CRIU
// has written to the pidFile.
func getRestorePid(pidFile string) (int, error) {
	var (
		bytes      []byte
		restorePid int
		err        error
	)

	if bytes, err = ioutil.ReadFile(pidFile); err != nil {
		log.Debugf("Cannot read %s: %s", pidFile, err)
		return 0, err
	}

	if restorePid, err = strconv.Atoi(strings.TrimSpace(string(bytes))); err != nil {
		log.Debugf("Cannot convert %s to int: %s", string(bytes), err)
		return 0, err
	}

	return restorePid, nil
}

// Update the PID field of the container's state file
// and write it out.
func updateState(restorePid int, criuImageDir, dataPath string) error {
	var (
		state *libcontainer.State
		err   error
	)

	if state, err = libcontainer.GetState(criuImageDir); err != nil {
		log.CRDbg("Cannot get state: %s", err)
		return err
	}

	state.InitPid = restorePid

	// XXX Do we need this (dataPath should already exist)?
	log.CRDbg("dataPath=%s", dataPath)
	if err = os.MkdirAll(dataPath, 0755); err != nil {
		log.CRDbg("Cannot mkdir -p %s: %s", dataPath, err)
		return err
	}

	if err = libcontainer.SaveState(dataPath, state); err != nil {
		log.CRDbg("Cannot save state: %s", err)
	}

	return err
}

// We get here through initializer() in daemon/execdriver/native/init.go
// after the command was started in Restore() above.
func InitRestore(pipe *os.File, args []string) (err error) {
	defer func() {
		if err != nil {
			ioutil.ReadAll(pipe)
			if err := json.NewEncoder(pipe).Encode(initError{
				Message: err.Error(),
			}); err != nil {
				panic(err)
			}
		}
		pipe.Close()
	}()

	// We can proceed to Execv() only after we've read a sync
	// message from the parent.
	var s syncMsg
	if err := json.NewDecoder(pipe).Decode(&s); err != nil {
		return err
	}

	return system.Execv(args[0], args[0:], os.Environ())
}
