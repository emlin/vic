// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package portlayer

import (
	"fmt"
	"path"

	"github.com/vmware/vic/lib/guest"
	"github.com/vmware/vic/lib/portlayer/attach"
	"github.com/vmware/vic/lib/portlayer/exec"
	"github.com/vmware/vic/lib/portlayer/logging"
	"github.com/vmware/vic/lib/portlayer/network"
	"github.com/vmware/vic/lib/portlayer/storage"
	"github.com/vmware/vic/lib/portlayer/store"
	"github.com/vmware/vic/pkg/retry"
	"github.com/vmware/vic/pkg/trace"
	"github.com/vmware/vic/pkg/vsphere/datastore"
	"github.com/vmware/vic/pkg/vsphere/extraconfig"
	"github.com/vmware/vic/pkg/vsphere/session"
	"github.com/vmware/vic/pkg/vsphere/vm"

	"context"

	log "github.com/Sirupsen/logrus"
)

// API defines the interface the REST server used by the portlayer expects the
// implementation side to export
type API interface {
	storage.ImageStorer
	storage.VolumeStorer
}

func Init(ctx context.Context, sess *session.Session) error {
	source, err := extraconfig.GuestInfoSource()
	if err != nil {
		return err
	}

	sink, err := extraconfig.GuestInfoSink()
	if err != nil {
		return err
	}

	// Grab the storage layer config blobs from extra config
	extraconfig.Decode(source, &storage.Config)
	log.Debugf("Decoded VCH config for storage: %#v", storage.Config)

	// create or restore a portlayer k/v store in the VCH's directory.
	vch, err := guest.GetSelf(ctx, sess)
	if err != nil {
		return err
	}

	// initialize error handler in tasks package, before actually query containers from vsphere
	exec.InitTasksErrorHandler()

	vchvm := vm.NewVirtualMachineFromVM(ctx, sess, vch)
	vmPath, err := vchvm.VMPathName(ctx)
	if err != nil {
		return err
	}

	// vmPath is set to the vmx.  Grab the directory from that.
	vmFolder, err := datastore.ToURL(path.Dir(vmPath))
	if err != nil {
		return err
	}

	if err = store.Init(ctx, sess, vmFolder); err != nil {
		return err
	}

	if err := exec.Init(ctx, sess, source, sink); err != nil {
		return err
	}

	if err = network.Init(ctx, sess, source, sink); err != nil {
		return err
	}

	if err = logging.Init(ctx); err != nil {
		return err
	}

	// Unbind containerVM serial ports configured with the old VCH IP.
	// Useful when the appliance restarts and the VCH has a different IP.
	TakeCareOfSerialPorts(sess)

	return nil
}

// TakeCareOfSerialPorts disconnects serial ports backed by network on the VCH's old IP and connects serial ports backed by file.
// This is useful when the appliance or the portlayer restarts and the VCH has a new IP or container vms gets migrated
// Any errors are logged and portlayer init proceeds as usual.
func TakeCareOfSerialPorts(sess *session.Session) {
	defer trace.End(trace.Begin(""))

	ctx := context.Background()

	// Get all running containers from the portlayer cache
	runningState := new(exec.State)
	*runningState = exec.StateRunning
	containers := exec.Containers.Containers(runningState)

	for i := range containers {
		var containerID string

		if containers[i].ExecConfig != nil {
			containerID = containers[i].ExecConfig.ID
		}
		log.Infof("unbinding serial port for running container %s", containerID)

		operation := func() error {
			// Obtain a container handle
			handle := containers[i].NewHandle(ctx)
			if handle == nil {
				err := fmt.Errorf("unable to obtain a handle for container %s", containerID)
				log.Error(err)

				return err
			}

			// Unbind the network backed VirtualSerialPort
			unbindHandle, err := attach.Unbind(handle)
			if err != nil {
				err := fmt.Errorf("unable to unbind serial port for container %s: %s", containerID, err)
				log.Error(err)

				return err
			}

			execHandle, ok := unbindHandle.(*exec.Handle)
			if !ok {
				err := fmt.Errorf("handle type assertion failed for container %s", containerID)
				log.Error(err)

				return err
			}

			// Bind the file backed VirtualSerialPort
			bindHandle, err := logging.Bind(execHandle)
			if err != nil {
				err := fmt.Errorf("unable to unbind serial port for container %s: %s", containerID, err)
				log.Error(err)

				return err
			}

			execHandle, ok = bindHandle.(*exec.Handle)
			if !ok {
				err := fmt.Errorf("handle type assertion failed for container %s", containerID)
				log.Error(err)

				return err
			}

			// Commit the handle
			if err := execHandle.Commit(ctx, sess, nil); err != nil {
				log.Errorf("unable to commit handle for container %s: %s", containerID, err)
				return err
			}
			return nil
		}

		if err := retry.Do(operation, exec.IsConcurrentAccessError); err != nil {
			log.Errorf("Multiple attempts failed for committing the handle with %s", err)
		}
	}
}
