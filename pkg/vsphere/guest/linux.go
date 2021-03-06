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

package guest

import (
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/vic/pkg/vsphere/session"
	"github.com/vmware/vic/pkg/vsphere/spec"
	"golang.org/x/net/context"
)

const (
	linuxGuestID = "other3xLinux64Guest"

	scsiBusNumber = 0
	scsiKey       = 100
	ideKey        = 200

	UUIDPath   = "/sys/class/dmi/id/product_serial"
	UUIDPrefix = "VMware-"
)

// LinuxGuestType type
type LinuxGuestType struct {
	*spec.VirtualMachineConfigSpec

	// holds the controller so that we don't end up calling
	// FindIDEController or FindSCSIController
	controller types.BaseVirtualController
}

// NewLinuxGuest returns a new Linux guest spec with predefined values
func NewLinuxGuest(ctx context.Context, session *session.Session, config *spec.VirtualMachineConfigSpecConfig) (Guest, error) {
	s, err := spec.NewVirtualMachineConfigSpec(ctx, session, config)
	if err != nil {
		return nil, err
	}

	// SCSI controller
	scsi := spec.NewVirtualSCSIController(scsiBusNumber, scsiKey)
	// PV SCSI controller
	pv := spec.NewParaVirtualSCSIController(scsi)
	s.AddParaVirtualSCSIController(pv)

	// Disk
	disk := spec.NewVirtualSCSIDisk(scsi)
	s.AddVirtualDisk(disk)

	// IDE controller
	ide := spec.NewVirtualIDEController(ideKey)
	s.AddVirtualIDEController(ide)

	// CDROM
	cdrom := spec.NewVirtualCdrom(ide)
	s.AddVirtualCdrom(cdrom)

	// NIC
	vmxnet3 := spec.NewVirtualVmxnet3()
	s.AddVirtualVmxnet3(vmxnet3)

	// Tether serial port - backed by network
	serial := spec.NewVirtualSerialPort()
	s.AddVirtualConnectedSerialPort(serial)

	// Debug serial port - backed by datastore file
	debugserial := spec.NewVirtualSerialPort()
	s.AddVirtualFileSerialPort(debugserial, "debug")

	// Session log serial port - backed by datastore file
	sessionserial := spec.NewVirtualSerialPort()
	s.AddVirtualFileSerialPort(sessionserial, "log")

	// Set the guest id
	s.GuestId = linuxGuestID

	return &LinuxGuestType{
		VirtualMachineConfigSpec: s,
		controller:               &scsi,
	}, nil
}

// GuestID returns the guest id of the linux guest
func (l *LinuxGuestType) GuestID() string {
	return l.VirtualMachineConfigSpec.GuestId
}

// Spec returns the underlying types.VirtualMachineConfigSpec to the caller
func (l *LinuxGuestType) Spec() *types.VirtualMachineConfigSpec {
	return l.VirtualMachineConfigSpec.VirtualMachineConfigSpec
}

// Controller returns the types.BaseVirtualController to the caller
func (l *LinuxGuestType) Controller() *types.BaseVirtualController {
	return &l.controller
}

// UUID gets the BIOS UUID via the sys interface.  This UUID is known by vphsere
func UUID() (string, error) {
	id, err := ioutil.ReadFile(UUIDPath)
	if err != nil {
		return "", err
	}

	uuidstr := string(id[:])

	// check the uuid starts with "VMware-"
	if !strings.HasPrefix(uuidstr, UUIDPrefix) {
		return "", fmt.Errorf("cannot find this VM's UUID")
	}

	// Strip the prefix, white spaces, and the trailing '\n'
	uuidstr = strings.Replace(uuidstr[len(UUIDPrefix):(len(uuidstr)-1)], " ", "", -1)

	// need to add dashes, e.g. "564d395e-d807-e18a-cb25-b79f65eb2b9f"
	uuidstr = fmt.Sprintf("%s-%s-%s-%s", uuidstr[0:8], uuidstr[8:12], uuidstr[12:21], uuidstr[21:])

	return uuidstr, nil
}

// GetSelf gets VirtualMachine reference for the VM this process is running on
func GetSelf(ctx context.Context, s *session.Session) (*object.VirtualMachine, error) {
	u, err := UUID()
	if err != nil {
		return nil, err
	}

	search := object.NewSearchIndex(s.Vim25())
	ref, err := search.FindByUuid(ctx, s.Datacenter, u, true, nil)
	if err != nil {
		return nil, err
	}

	if ref == nil {
		return nil, fmt.Errorf("can't find the hosting vm")
	}

	vm := object.NewVirtualMachine(s.Client.Client, ref.Reference())
	return vm, nil
}
