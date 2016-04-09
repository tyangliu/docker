package daemon

import (
	"os"
	"path"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/container"
	derr "github.com/docker/docker/errors"
	"github.com/docker/docker/layer"
	volumestore "github.com/docker/docker/volume/store"
	"github.com/docker/engine-api/types"
)

// ContainerRm removes the container id from the filesystem. An error
// is returned if the container is not found, or if the remove
// fails. If the remove succeeds, the container name is released, and
// network links are removed.
func (daemon *Daemon) ContainerRm(name string, config *types.ContainerRmConfig) error {
	container, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	// Container state RemovalInProgress should be used to avoid races.
	if err = container.SetRemovalInProgress(); err != nil {
		if err == derr.ErrorCodeAlreadyRemoving {
			// do not fail when the removal is in progress started by other request.
			return nil
		}
		return derr.ErrorCodeRmState.WithArgs(err)
	}
	defer container.ResetRemovalInProgress()

	// check if container wasn't deregistered by previous rm since Get
	if c := daemon.containers.Get(container.ID); c == nil {
		return nil
	}

	if config.RemoveLink {
		return daemon.rmLink(name)
	}

	if err := daemon.cleanupContainer(container, config.ForceRemove); err != nil {
		// return derr.ErrorCodeCantDestroy.WithArgs(name, utils.GetErrorMessage(err))
		return err
	}

	if err := daemon.removeMountPoints(container, config.RemoveVolume); err != nil {
		logrus.Error(err)
	}

	return nil
}

// rmLink removes link by name from other containers
func (daemon *Daemon) rmLink(name string) error {
	name, err := GetFullContainerName(name)
	if err != nil {
		return err
	}
	parent, n := path.Split(name)
	if parent == "/" {
		return derr.ErrorCodeDefaultName
	}
	pe := daemon.containerGraph().Get(parent)
	if pe == nil {
		return derr.ErrorCodeNoParent.WithArgs(parent, name)
	}

	if err := daemon.containerGraph().Delete(name); err != nil {
		return err
	}

	parentContainer, _ := daemon.GetContainer(pe.ID())
	if parentContainer != nil {
		if err := daemon.updateNetwork(parentContainer); err != nil {
			logrus.Debugf("Could not update network to remove link %s: %v", n, err)
		}
	}

	return nil
}

// cleanupContainer unregisters a container from the daemon, stops stats
// collection and cleanly removes contents and metadata from the filesystem.
func (daemon *Daemon) cleanupContainer(container *container.Container, forceRemove bool) (err error) {
	if container.IsRunning() {
		if !forceRemove {
			return derr.ErrorCodeRmRunning
		}
		if err := daemon.Kill(container); err != nil {
			return derr.ErrorCodeRmFailed.WithArgs(err)
		}
	}

	// stop collection of stats for the container regardless
	// if stats are currently getting collected.
	daemon.statsCollector.stopCollection(container)

	if err = daemon.containerStop(container, 3); err != nil {
		return err
	}

	// Mark container dead. We don't want anybody to be restarting it.
	container.SetDead()

	// Save container state to disk. So that if error happens before
	// container meta file got removed from disk, then a restart of
	// docker should not make a dead container alive.
	if err := container.ToDiskLocking(); err != nil {
		logrus.Errorf("Error saving dying container to disk: %v", err)
	}

	// If force removal is required, delete container from various
	// indexes even if removal failed.
	defer func() {
		if err == nil || forceRemove {
			if _, err := daemon.containerGraphDB.Purge(container.ID); err != nil {
				logrus.Debugf("Unable to remove container from link graph: %s", err)
			}
			selinuxFreeLxcContexts(container.ProcessLabel)
			daemon.idIndex.Delete(container.ID)
			daemon.containers.Delete(container.ID)
			daemon.LogContainerEvent(container, "destroy")
		}
	}()

	// Try a maximum of 10 times to remove the container's
	// root directory.
	// XXX This is just a workaround.  We need to find out why
	//     os.RemoveAll() can return with an ENOTEMPTY error.
	var i int
	for i = 0; i < 10; i++ {
		if err = os.RemoveAll(container.Root); err == nil {
			break
		}
		if err.(*os.PathError).Err != syscall.ENOTEMPTY {
			return derr.ErrorCodeRmFS.WithArgs(container.ID, err)
		}
		logrus.Debugf(">>> Trying RemoveAll() again...\n")
	}
	if i == 10 {
		return derr.ErrorCodeRmFS.WithArgs(container.ID, err)
	}

	metadata, err := daemon.layerStore.ReleaseRWLayer(container.RWLayer)
	layer.LogReleaseMetadata(metadata)
	if err != nil && err != layer.ErrMountDoesNotExist {
		return derr.ErrorCodeRmDriverFS.WithArgs(daemon.GraphDriverName(), container.ID, err)
	}

	if err = daemon.execDriver.Clean(container.ID); err != nil {
		return derr.ErrorCodeRmExecDriver.WithArgs(container.ID, err)
	}

	return nil
}

// VolumeRm removes the volume with the given name.
// If the volume is referenced by a container it is not removed
// This is called directly from the remote API
func (daemon *Daemon) VolumeRm(name string) error {
	v, err := daemon.volumes.Get(name)
	if err != nil {
		return err
	}

	if err := daemon.volumes.Remove(v); err != nil {
		if volumestore.IsInUse(err) {
			return derr.ErrorCodeRmVolumeInUse.WithArgs(err)
		}
		return derr.ErrorCodeRmVolume.WithArgs(name, err)
	}
	daemon.LogVolumeEvent(v.Name(), "destroy", map[string]string{"driver": v.DriverName()})
	return nil
}
