// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package wrappers

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/systemd"
	"github.com/snapcore/snapd/timeout"
)

type interacter interface {
	Notify(status string)
}

// wait this time between TERM and KILL
var killWait = 5 * time.Second

func serviceStopTimeout(app *snap.AppInfo) time.Duration {
	tout := app.StopTimeout
	if tout == 0 {
		tout = timeout.DefaultTimeout
	}
	return time.Duration(tout)
}

func generateSnapServiceFile(app *snap.AppInfo) ([]byte, error) {
	if err := snap.ValidateApp(app); err != nil {
		return nil, err
	}

	return genServiceFile(app), nil
}

func stopService(sysd systemd.Systemd, app *snap.AppInfo, inter interacter) error {
	serviceName := app.ServiceName()
	tout := serviceStopTimeout(app)

	socketErrors := []error{}
	for _, socket := range app.Sockets {
		if err := sysd.Stop(filepath.Base(socket.File()), tout); err != nil {
			socketErrors = append(socketErrors, err)
		}
	}

	if err := sysd.Stop(serviceName, tout); err != nil {
		if !systemd.IsTimeout(err) {
			return err
		}
		inter.Notify(fmt.Sprintf("%s refused to stop, killing.", serviceName))
		// ignore errors for kill; nothing we'd do differently at this point
		sysd.Kill(serviceName, "TERM")
		time.Sleep(killWait)
		sysd.Kill(serviceName, "KILL")

	}

	if len(socketErrors) > 0 {
		return socketErrors[0]
	}

	return nil
}

// StartServices starts service units for the applications from the snap which are services.
func StartServices(apps []*snap.AppInfo, inter interacter) (err error) {
	sysd := systemd.New(dirs.GlobalRootDir, inter)

	services := make([]string, 0, len(apps))
	for _, app := range apps {
		// they're *supposed* to be all services, but checking doesn't hurt
		if !app.IsService() {
			continue
		}

		if len(app.Sockets) == 0 {
			services = append(services, app.ServiceName())
		}

		for _, socket := range app.Sockets {
			socketService := filepath.Base(socket.File())
			// enable the socket
			if err := sysd.Enable(socketService); err != nil {
				return err
			}

			if err := sysd.Start(socketService); err != nil {
				return err
			}
		}

		defer func(app *snap.AppInfo) {
			if err == nil {
				return
			}
			if e := stopService(sysd, app, inter); e != nil {
				inter.Notify(fmt.Sprintf("While trying to stop previously started service %q: %v", app.ServiceName(), e))
			}
		}(app)
	}

	if len(services) > 0 {
		if err := sysd.Start(services...); err != nil {
			// cleanup was set up by iterating over apps
			return err
		}
	}

	return nil
}

// AddSnapServices adds service units for the applications from the snap which are services.
func AddSnapServices(s *snap.Info, inter interacter) (err error) {
	sysd := systemd.New(dirs.GlobalRootDir, inter)
	var written []string
	var enabled []string
	defer func() {
		if err == nil {
			return
		}
		for _, s := range enabled {
			if e := sysd.Disable(s); e != nil {
				inter.Notify(fmt.Sprintf("while trying to disable %s due to previous failure: %v", s, e))
			}
		}
		for _, s := range written {
			if e := os.Remove(s); e != nil {
				inter.Notify(fmt.Sprintf("while trying to remove %s due to previous failure: %v", s, e))
			}
		}
		if len(written) > 0 {
			if e := sysd.DaemonReload(); e != nil {
				inter.Notify(fmt.Sprintf("while trying to perform systemd daemon-reload due to previous failure: %v", e))
			}
		}
	}()

	for _, app := range s.Apps {
		if !app.IsService() {
			continue
		}
		// Generate service file
		content, err := generateSnapServiceFile(app)
		if err != nil {
			return err
		}
		svcFilePath := app.ServiceFile()
		os.MkdirAll(filepath.Dir(svcFilePath), 0755)
		if err := osutil.AtomicWriteFile(svcFilePath, content, 0644, 0); err != nil {
			return err
		}
		written = append(written, svcFilePath)

		// Generate systemd .socket files if needed
		socketFiles, err := generateSnapSocketFiles(app)
		if err != nil {
			return err
		}
		for path, content := range *socketFiles {
			os.MkdirAll(filepath.Dir(path), 0755)
			if err := osutil.AtomicWriteFile(path, content, 0644, 0); err != nil {
				return err
			}
			written = append(written, path)
		}

		svcName := app.ServiceName()
		if err := sysd.Enable(svcName); err != nil {
			return err
		}
		enabled = append(enabled, svcName)
	}

	if len(enabled) > 0 {
		if err := sysd.DaemonReload(); err != nil {
			return err
		}
	}

	return nil
}

// StopServices stops service units for the applications from the snap which are services.
func StopServices(apps []*snap.AppInfo, inter interacter) error {
	sysd := systemd.New(dirs.GlobalRootDir, inter)

	for _, app := range apps {
		// Handle the case where service file doesn't exist and don't try to stop it as it will fail.
		// This can happen with snap try when snap.yaml is modified on the fly and a daemon line is added.
		if !app.IsService() || !osutil.FileExists(app.ServiceFile()) {
			continue
		}
		if err := stopService(sysd, app, inter); err != nil {
			return err
		}
	}

	return nil

}

// RemoveSnapServices disables and removes service units for the applications from the snap which are services.
func RemoveSnapServices(s *snap.Info, inter interacter) error {
	sysd := systemd.New(dirs.GlobalRootDir, inter)
	nservices := 0

	for _, app := range s.Apps {
		if !app.IsService() || !osutil.FileExists(app.ServiceFile()) {
			continue
		}
		nservices++

		serviceName := filepath.Base(app.ServiceFile())

		for _, socket := range app.Sockets {
			path := socket.File()
			socketServiceName := filepath.Base(path)
			if err := sysd.Disable(socketServiceName); err != nil {
				return err
			}

			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				logger.Noticef("Failed to remove socket file %q for %q: %v", path, serviceName, err)
			}
		}

		if err := sysd.Disable(serviceName); err != nil {
			return err
		}

		if err := os.Remove(app.ServiceFile()); err != nil && !os.IsNotExist(err) {
			logger.Noticef("Failed to remove service file for %q: %v", serviceName, err)
		}

	}

	// only reload if we actually had services
	if nservices > 0 {
		if err := sysd.DaemonReload(); err != nil {
			return err
		}
	}

	return nil
}

func genServiceNames(snap *snap.Info, appNames []string) []string {
	names := make([]string, 0, len(appNames))

	for _, name := range appNames {
		if app := snap.Apps[name]; app != nil {
			names = append(names, app.ServiceName())
		}
	}
	return names
}

func genServiceFile(appInfo *snap.AppInfo) []byte {
	serviceTemplate := `[Unit]
# Auto-generated, DO NOT EDIT
Description=Service for snap application {{.App.Snap.Name}}.{{.App.Name}}
Requires={{.MountUnit}}
Wants={{.PrerequisiteTarget}}
After={{.MountUnit}} {{.PrerequisiteTarget}}{{range .After}} {{.}}{{end}}
{{- if .Before}}
Before={{ range .Before -}}{{.}} {{- end}}
{{- end}}
X-Snappy=yes

[Service]
ExecStart={{.App.LauncherCommand}}
SyslogIdentifier={{.App.Snap.Name}}.{{.App.Name}}
Restart={{.Restart}}
WorkingDirectory={{.App.Snap.DataDir}}
{{- if .App.StopCommand}}
ExecStop={{.App.LauncherStopCommand}}
{{- end}}
{{- if .App.ReloadCommand}}
ExecReload={{.App.LauncherReloadCommand}}
{{- end}}
{{- if .App.PostStopCommand}}
ExecStopPost={{.App.LauncherPostStopCommand}}
{{- end}}
{{- if .StopTimeout}}
TimeoutStopSec={{.StopTimeout.Seconds}}
{{- end}}
Type={{.App.Daemon}}
{{- if .Remain}}
RemainAfterExit={{.Remain}}
{{- end}}
{{- if .App.BusName}}
BusName={{.App.BusName}}
{{- end}}
{{- if not .App.Sockets}}

[Install]
WantedBy={{.ServicesTarget}}
{{- end}}
`
	var templateOut bytes.Buffer
	t := template.Must(template.New("service-wrapper").Parse(serviceTemplate))

	restartCond := appInfo.RestartCond.String()
	if restartCond == "" {
		restartCond = snap.RestartOnFailure.String()
	}

	var remain string
	if appInfo.Daemon == "oneshot" {
		// any restart condition other than "no" is invalid for oneshot daemons
		restartCond = "no"
		// If StopExec is present for a oneshot service than we also need
		// RemainAfterExit=yes
		if appInfo.StopCommand != "" {
			remain = "yes"
		}
	}

	wrapperData := struct {
		App *snap.AppInfo

		Restart            string
		StopTimeout        time.Duration
		ServicesTarget     string
		PrerequisiteTarget string
		MountUnit          string
		Remain             string
		Before             []string
		After              []string

		Home    string
		EnvVars string
	}{
		App: appInfo,

		Restart:            restartCond,
		StopTimeout:        serviceStopTimeout(appInfo),
		ServicesTarget:     systemd.ServicesTarget,
		PrerequisiteTarget: systemd.PrerequisiteTarget,
		MountUnit:          filepath.Base(systemd.MountUnitPath(appInfo.Snap.MountDir())),
		Remain:             remain,
		Before:             genServiceNames(appInfo.Snap, appInfo.Before),
		After:              genServiceNames(appInfo.Snap, appInfo.After),

		// systemd runs as PID 1 so %h will not work.
		Home: "/root",
	}

	if err := t.Execute(&templateOut, wrapperData); err != nil {
		// this can never happen, except we forget a variable
		logger.Panicf("Unable to execute template: %v", err)
	}

	return templateOut.Bytes()
}

func genServiceSocketFile(appInfo *snap.AppInfo, socketName string) []byte {
	socketTemplate := `[Unit]
# Auto-generated, DO NO EDIT
Description=Socket {{.SocketName}} for snap application {{.App.Snap.Name}}.{{.App.Name}}
Requires={{.MountUnit}}
Wants={{.PrerequisiteTarget}}
After={{.MountUnit}} {{.PrerequisiteTarget}}
X-Snappy=yes

[Socket]
Service={{.ServiceFileName}}
FileDescriptorName={{.SocketInfo.Name}}
ListenStream={{.ListenStream}}
{{if .SocketInfo.SocketMode}}SocketMode={{.SocketInfo.SocketMode | printf "%04o"}}{{end}}

[Install]
WantedBy={{.SocketsTarget}}
`
	var templateOut bytes.Buffer
	t := template.Must(template.New("socket-wrapper").Parse(socketTemplate))

	socket := appInfo.Sockets[socketName]
	listenStream := renderListenStream(socket)
	wrapperData := struct {
		App                *snap.AppInfo
		ServiceFileName    string
		PrerequisiteTarget string
		SocketsTarget      string
		MountUnit          string
		SocketName         string
		SocketInfo         *snap.SocketInfo
		ListenStream       string
	}{
		App:                appInfo,
		ServiceFileName:    filepath.Base(appInfo.ServiceFile()),
		SocketsTarget:      systemd.SocketsTarget,
		PrerequisiteTarget: systemd.PrerequisiteTarget,
		MountUnit:          filepath.Base(systemd.MountUnitPath(appInfo.Snap.MountDir())),
		SocketName:         socketName,
		SocketInfo:         socket,
		ListenStream:       listenStream,
	}

	if err := t.Execute(&templateOut, wrapperData); err != nil {
		// this can never happen, except we forget a variable
		logger.Panicf("Unable to execute template: %v", err)
	}

	return templateOut.Bytes()
}

func generateSnapSocketFiles(app *snap.AppInfo) (*map[string][]byte, error) {
	if err := snap.ValidateApp(app); err != nil {
		return nil, err
	}

	socketFiles := make(map[string][]byte)
	for name, socket := range app.Sockets {
		socketFiles[socket.File()] = genServiceSocketFile(app, name)
	}
	return &socketFiles, nil
}

func renderListenStream(socket *snap.SocketInfo) string {
	snap := socket.App.Snap
	listenStream := strings.Replace(socket.ListenStream, "$SNAP_DATA", snap.DataDir(), -1)
	return strings.Replace(listenStream, "$SNAP_COMMON", snap.CommonDataDir(), -1)
}
