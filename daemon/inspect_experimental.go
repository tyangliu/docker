// +build experimental

package daemon

import (
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/container"
)

func addExperimentalState(container *container.Container, data *types.ContainerStateBase) *types.ContainerState {
	containerState := &types.ContainerState{}
	containerState.ContainerStateBase = *data
	containerState.Checkpointed = container.State.Checkpointed
	containerState.CheckpointedAt = container.State.CheckpointedAt.Format(time.RFC3339Nano)
	return containerState
}
