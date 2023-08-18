package system

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"

	"github.com/home-assistant/os-agent/udisks2"
	logging "github.com/home-assistant/os-agent/utils/log"
)

const (
	objectPath               = "/io/hass/os/System"
	ifaceName                = "io.hass.os.System"
	labelDataFileSystem      = "hassos-data"
	labelOverlayFileSystem   = "hassos-overlay"
	kernelCommandLine        = "/mnt/boot/cmdline.txt"
	tmpKernelCommandLine     = "/mnt/boot/.tmp.cmdline.txt"
	sshAuthKeyFileName       = "/root/.ssh/authorized_keys"
	modulesAutoloadDirectory = "/etc/modules-load.d/"
	moduleLoadCommand        = "/sbin/modprobe"
)

var (
	loadUSBIP bool
)

type system struct {
	conn  *dbus.Conn
	props *prop.Properties
}

func getAndCheckBusObjectFromLabel(udisks2helper udisks2.UDisks2Helper, label string) (dbus.BusObject, error) {
	dataBusObject, err := udisks2helper.GetBusObjectFromLabel(label)
	if err != nil {
		return nil, dbus.MakeFailedError(err)
	}

	dataFilesystem := udisks2.NewFilesystem(dataBusObject)
	dataMountPoints, err := dataFilesystem.GetMountPointsString(context.Background())
	if err != nil {
		return nil, dbus.MakeFailedError(err)
	}

	if len(dataMountPoints) > 0 {
		return nil, dbus.MakeFailedError(fmt.Errorf("Device with label \"%s\" is mounted at %s, aborting.", label, dataMountPoints))
	}

	return dataBusObject, nil
}

func (d system) WipeDevice() (bool, *dbus.Error) {
	logging.Info.Printf("Wipe device data.")

	udisks2helper := udisks2.NewUDisks2(d.conn)
	dataBusObject, err := getAndCheckBusObjectFromLabel(udisks2helper, labelDataFileSystem)
	if err != nil {
		return false, dbus.MakeFailedError(err)
	}

	overlayBusObject, err := getAndCheckBusObjectFromLabel(udisks2helper, labelOverlayFileSystem)
	if err != nil {
		return false, dbus.MakeFailedError(err)
	}

	err = udisks2helper.FormatPartition(dataBusObject, "ext4", labelDataFileSystem)
	if err != nil {
		return false, dbus.MakeFailedError(err)
	}
	err = udisks2helper.FormatPartition(overlayBusObject, "ext4", labelOverlayFileSystem)
	if err != nil {
		return false, dbus.MakeFailedError(err)
	}
	logging.Info.Printf("Successfully wiped device data.")

	return true, nil
}

func (d system) ScheduleWipeDevice() (bool, *dbus.Error) {

	data, err := ioutil.ReadFile(kernelCommandLine)
	if err != nil {
		fmt.Println(err)
		return false, dbus.MakeFailedError(err)
	}

	datastr := strings.TrimSpace(string(data))
	datastr += " haos.wipe=1"

	err = ioutil.WriteFile(tmpKernelCommandLine, []byte(datastr), 0644)
	if err != nil {
		fmt.Println(err)
		return false, dbus.MakeFailedError(err)
	}

	// Boot is mounted sync on Home Assistant OS, so just rename should be fine.
	err = os.Rename(tmpKernelCommandLine, kernelCommandLine)
	if err != nil {
		fmt.Println(err)
		return false, dbus.MakeFailedError(err)
	}

	logging.Info.Printf("Device will get wiped on next reboot!")
	return true, nil
}

func (d system) AddSSHAuthKey(newKey string) *dbus.Error {

	file, err := os.OpenFile(sshAuthKeyFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logging.Error.Printf("Failed to open SSH authentication file %s: %s", sshAuthKeyFileName, err)
		return dbus.MakeFailedError(err)
	}

	defer file.Close()

	if _, err := file.WriteString(newKey + "\n"); err != nil {
		logging.Error.Printf("Failed to write SSH authentication file: %s.", err)
		return dbus.MakeFailedError(err)
	}

	logging.Info.Printf("New SSH authentication key added for user root.")

	return nil
}

func (d system) ClearSSHAuthKeys() *dbus.Error {
	if err := os.Remove(sshAuthKeyFileName); err != nil && os.IsNotExist(err) {
		logging.Error.Printf("Failed to delete SSH authentication file %s: %s", sshAuthKeyFileName, err)
		return dbus.MakeFailedError(err)
	}

	return nil
}

func getDriverStatus() bool {
	cmd := "cat /proc/modules | grep vhci-hcd"
	out, err := exec.Command(cmd).Output()
	if err != nil {
		return false
	}
	value := strings.SplitN(string(out), " ", 2)[0]
	return value == "vhci-hcd"
}

func LoadKernelDriver(c *prop.Change) *dbus.Error {
	logging.Info.Printf("Loading usbip driver: %t", c.Value)
	loadUSBIP = c.Value.(bool)

	var err error
	cmd := exec.Command(moduleLoadCommand)
	if c.Value.(bool) {
		cmd.Args = append(cmd.Args, "vhci-hcd")
	} else {
		cmd.Args = append(cmd.Args, "--remove", "vhci-hcd")
	}
	_, cerror := cmd.StdinPipe()

	if cerror != nil {
		return dbus.MakeFailedError(err)
	}
	return nil
}

func InitializeDBus(conn *dbus.Conn) {
	d := system{
		conn: conn,
	}

	loadUSBIP = getDriverStatus()

	propsSpec := map[string]map[string]*prop.Prop{
		ifaceName: {
			"LoadUSBIP": {
				Value:    loadUSBIP,
				Writable: true,
				Emit:     prop.EmitTrue,
				Callback: LoadKernelDriver,
			},
		},
	}

	props, err := prop.Export(conn, objectPath, propsSpec)
	if err != nil {
		logging.Critical.Panic(err)
	}
	d.props = props

	err = conn.Export(d, objectPath, ifaceName)
	if err != nil {
		logging.Critical.Panic(err)
	}

	node := &introspect.Node{
		Name: objectPath,
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name:    ifaceName,
				Methods: introspect.Methods(d),
			},
		},
	}

	err = conn.Export(introspect.NewIntrospectable(node), objectPath, "org.freedesktop.DBus.Introspectable")
	if err != nil {
		logging.Critical.Panic(err)
	}

	logging.Info.Printf("Exposing object %s with interface %s ...", objectPath, ifaceName)
}
