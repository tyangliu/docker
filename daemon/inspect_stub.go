// +build !experimental

package daemon

import (
	"github.com/docker/engine-api/types"
	"github.com/docker/docker/container"
)

func addExperimentalState(container *container.Container, data *types.ContainerStateBase) *types.ContainerState {
	return &types.ContainerState{ContainerStateBase: *data}
}
