// +build !experimental

package daemon

import (
	"github.com/docker/docker/container"
    "github.com/docker/engine-api/types"
)

func addExperimentalState(container *container.Container, data *types.ContainerStateBase) *types.ContainerState {
	return &types.ContainerState{ContainerStateBase: *data}
}
