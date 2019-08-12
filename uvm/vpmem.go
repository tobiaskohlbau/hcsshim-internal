package uvm

import (
	"context"
	"fmt"

	"github.com/tobiaskohlbau/hcsshim-internal/guestrequest"
	"github.com/tobiaskohlbau/hcsshim-internal/log"
	"github.com/tobiaskohlbau/hcsshim-internal/logfields"
	"github.com/tobiaskohlbau/hcsshim-internal/requesttype"
	hcsschema "github.com/tobiaskohlbau/hcsshim-internal/schema2"
	"github.com/sirupsen/logrus"
)

// allocateVPMEM finds the next available VPMem slot. The lock MUST be held
// when calling this function.
func (uvm *UtilityVM) allocateVPMEM(ctx context.Context, hostPath string) (uint32, error) {
	for index, vi := range uvm.vpmemDevices {
		if vi.hostPath == "" {
			vi.hostPath = hostPath
			log.G(ctx).WithFields(logrus.Fields{
				logfields.UVMID: uvm.id,
				"host-path":     vi.hostPath,
				"uvm-path":      vi.uvmPath,
				"refCount":      vi.refCount,
				"deviceNumber":  uint32(index),
			}).Debug("uvm::allocateVPMEM")
			return uint32(index), nil
		}
	}
	return 0, fmt.Errorf("no free VPMEM locations")
}

// Lock must be held when calling this function
func (uvm *UtilityVM) findVPMEMDevice(ctx context.Context, findThisHostPath string) (uint32, string, error) {
	for deviceNumber, vi := range uvm.vpmemDevices {
		if vi.hostPath == findThisHostPath {
			log.G(ctx).WithFields(logrus.Fields{
				logfields.UVMID: uvm.id,
				"host-path":     findThisHostPath,
				"uvm-path":      vi.uvmPath,
				"refCount":      vi.refCount,
				"deviceNumber":  uint32(deviceNumber),
			}).Debug("uvm::findVPMEMDevice")
			return uint32(deviceNumber), vi.uvmPath, nil
		}
	}
	return 0, "", fmt.Errorf("%s is not attached to VPMEM", findThisHostPath)
}

// AddVPMEM adds a VPMEM disk to a utility VM at the next available location.
//
// Returns the location(0..MaxVPMEM-1) where the device is attached, and if exposed,
// the utility VM path which will be /tmp/p<location>//
func (uvm *UtilityVM) AddVPMEM(ctx context.Context, hostPath string, expose bool) (_ uint32, _ string, err error) {
	op := "uvm::AddVPMEM"
	l := log.G(ctx).WithFields(logrus.Fields{
		logfields.UVMID: uvm.id,
		"host-path":     hostPath,
		"expose":        expose,
	})
	l.Debug(op + " - Begin Operation")
	defer func() {
		if err != nil {
			l.Data[logrus.ErrorKey] = err
			l.Error(op + " - End Operation - Error")
		} else {
			l.Debug(op + " - End Operation - Success")
		}
	}()

	if uvm.operatingSystem != "linux" {
		return 0, "", errNotSupported
	}

	uvm.m.Lock()
	defer uvm.m.Unlock()

	var deviceNumber uint32
	uvmPath := ""

	deviceNumber, uvmPath, err = uvm.findVPMEMDevice(ctx, hostPath)
	if err != nil {
		// It doesn't exist, so we're going to allocate and hot-add it
		deviceNumber, err = uvm.allocateVPMEM(ctx, hostPath)
		if err != nil {
			return 0, "", err
		}

		modification := &hcsschema.ModifySettingRequest{
			RequestType: requesttype.Add,
			Settings: hcsschema.VirtualPMemDevice{
				HostPath:    hostPath,
				ReadOnly:    true,
				ImageFormat: "Vhd1",
			},
			ResourcePath: fmt.Sprintf("VirtualMachine/Devices/VirtualPMem/Devices/%d", deviceNumber),
		}

		if expose {
			uvmPath = fmt.Sprintf("/tmp/p%d", deviceNumber)
			modification.GuestRequest = guestrequest.GuestRequest{
				ResourceType: guestrequest.ResourceTypeVPMemDevice,
				RequestType:  requesttype.Add,
				Settings: guestrequest.LCOWMappedVPMemDevice{
					DeviceNumber: deviceNumber,
					MountPath:    uvmPath,
				},
			}
		}

		if err := uvm.Modify(ctx, modification); err != nil {
			uvm.vpmemDevices[deviceNumber] = vpmemInfo{}
			return 0, "", fmt.Errorf("uvm::AddVPMEM: failed to modify utility VM configuration: %s", err)
		}

		uvm.vpmemDevices[deviceNumber] = vpmemInfo{
			hostPath: hostPath,
			refCount: 1,
			uvmPath:  uvmPath}
	} else {
		pmemi := vpmemInfo{
			hostPath: hostPath,
			refCount: uvm.vpmemDevices[deviceNumber].refCount + 1,
			uvmPath:  uvmPath}
		uvm.vpmemDevices[deviceNumber] = pmemi
	}
	log.G(ctx).WithFields(logrus.Fields{
		logfields.UVMID: uvm.id,
		"device":        fmt.Sprintf("%+v", uvm.vpmemDevices[deviceNumber]),
	}).Debug("hcsshim::AddVPMEM Success")
	return deviceNumber, uvmPath, nil
}

// RemoveVPMEM removes a VPMEM disk from a utility VM.
func (uvm *UtilityVM) RemoveVPMEM(ctx context.Context, hostPath string) (err error) {
	op := "uvm::RemoveVPMEM"
	l := log.G(ctx).WithFields(logrus.Fields{
		logfields.UVMID: uvm.id,
		"host-path":     hostPath,
	})
	l.Debug(op + " - Begin Operation")
	defer func() {
		if err != nil {
			l.Data[logrus.ErrorKey] = err
			l.Error(op + " - End Operation - Error")
		} else {
			l.Debug(op + " - End Operation - Success")
		}
	}()

	if uvm.operatingSystem != "linux" {
		return errNotSupported
	}

	uvm.m.Lock()
	defer uvm.m.Unlock()

	// Make sure is actually attached
	deviceNumber, uvmPath, err := uvm.findVPMEMDevice(ctx, hostPath)
	if err != nil {
		return fmt.Errorf("cannot remove VPMEM %s as it is not attached to utility VM %s: %s", hostPath, uvm.id, err)
	}

	if uvm.vpmemDevices[deviceNumber].refCount == 1 {
		modification := &hcsschema.ModifySettingRequest{
			RequestType:  requesttype.Remove,
			ResourcePath: fmt.Sprintf("VirtualMachine/Devices/VirtualPMem/Devices/%d", deviceNumber),
			GuestRequest: guestrequest.GuestRequest{
				ResourceType: guestrequest.ResourceTypeVPMemDevice,
				RequestType:  requesttype.Remove,
				Settings: guestrequest.LCOWMappedVPMemDevice{
					DeviceNumber: deviceNumber,
					MountPath:    uvmPath,
				},
			},
		}

		if err := uvm.Modify(ctx, modification); err != nil {
			return fmt.Errorf("failed to remove VPMEM %s from utility VM %s: %s", hostPath, uvm.id, err)
		}
		uvm.vpmemDevices[deviceNumber] = vpmemInfo{}
		return nil
	}
	uvm.vpmemDevices[deviceNumber].refCount--
	return nil

}

// PMemMaxSizeBytes returns the maximum size of a PMEM layer (LCOW)
func (uvm *UtilityVM) PMemMaxSizeBytes() uint64 {
	return uvm.vpmemMaxSizeBytes
}
