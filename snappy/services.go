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

package snappy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ubuntu-core/snappy/dirs"
	"github.com/ubuntu-core/snappy/logger"
	"github.com/ubuntu-core/snappy/osutil"
	"github.com/ubuntu-core/snappy/snap"
	"github.com/ubuntu-core/snappy/systemd"
	"github.com/ubuntu-core/snappy/timeout"
)

type agreer interface {
	Agreed(intro, license string) bool
}

type interacter interface {
	agreer
	Notify(status string)
}

// wait this time between TERM and KILL
var killWait = 5 * time.Second

// servicesBinariesStringsWhitelist is the whitelist of legal chars
// in the "binaries" and "services" section of the snap.yaml
var servicesBinariesStringsWhitelist = regexp.MustCompile(`^[A-Za-z0-9/. _#:-]*$`)

func serviceStopTimeout(app *snap.AppInfo) time.Duration {
	tout := app.StopTimeout
	if tout == 0 {
		tout = timeout.DefaultTimeout
	}
	return time.Duration(tout)
}

func generateSnapServicesFile(app *snap.AppInfo, baseDir string, aaProfile string) (string, error) {
	if err := snap.ValidateApp(app); err != nil {
		return "", err
	}

	desc := fmt.Sprintf("service %s for snap %s - autogenerated DO NO EDIT", app.Name, app.Snap.Name)

	socketFileName := ""
	if app.Socket {
		socketFileName = filepath.Base(generateSocketFileName(app))
	}

	return systemd.New(dirs.GlobalRootDir, nil).GenServiceFile(
		&systemd.ServiceDescription{
			SnapName:       app.Snap.Name,
			AppName:        app.Name,
			Version:        app.Snap.Version,
			Description:    desc,
			SnapPath:       baseDir,
			Start:          app.Command,
			Stop:           app.Stop,
			PostStop:       app.PostStop,
			StopTimeout:    serviceStopTimeout(app),
			AaProfile:      aaProfile,
			BusName:        app.BusName,
			Type:           app.Daemon,
			UdevAppName:    fmt.Sprintf("%s.%s", app.Snap.Name, app.Name),
			Socket:         app.Socket,
			SocketFileName: socketFileName,
			Restart:        app.RestartCond,
		}), nil
}
func generateSnapSocketFile(app *snap.AppInfo, baseDir string, aaProfile string) (string, error) {
	if err := snap.ValidateApp(app); err != nil {
		return "", err
	}

	// lp: #1515709, systemd will default to 0666 if no socket mode
	// is specified
	if app.SocketMode == "" {
		app.SocketMode = "0660"
	}

	serviceFileName := filepath.Base(generateServiceFileName(app))

	return systemd.New(dirs.GlobalRootDir, nil).GenSocketFile(
		&systemd.ServiceDescription{
			ServiceFileName: serviceFileName,
			ListenStream:    app.ListenStream,
			SocketMode:      app.SocketMode,
		}), nil
}

func oldGenerateServiceFileName(m *snapYaml, app *AppYaml) string {
	return filepath.Join(dirs.SnapServicesDir, fmt.Sprintf("%s_%s_%s.service", m.Name, app.Name, m.Version))
}

func generateServiceFileName(app *snap.AppInfo) string {
	return filepath.Join(dirs.SnapServicesDir, fmt.Sprintf("%s_%s_%s.service", app.Snap.Name, app.Name, app.Snap.Version))
}

func generateSocketFileName(app *snap.AppInfo) string {
	return filepath.Join(dirs.SnapServicesDir, fmt.Sprintf("%s_%s_%s.socket", app.Snap.Name, app.Name, app.Snap.Version))
}

func generateBusPolicyFileName(app *snap.AppInfo) string {
	return filepath.Join(dirs.SnapBusPolicyDir, fmt.Sprintf("%s_%s_%s.conf", app.Snap.Name, app.Name, app.Snap.Version))
}

func addPackageServices(s *snap.Info, inter interacter) error {
	baseDir := s.BaseDir()

	for _, app := range s.Apps {
		if app.Daemon == "" {
			continue
		}
		aaProfile := getSecurityProfile2(s, app.Name, baseDir)
		// this will remove the global base dir when generating the
		// service file, this ensures that /snaps/foo/1.0/bin/start
		// is in the service file when the SetRoot() option
		// is used
		realBaseDir := stripGlobalRootDir(baseDir)
		// Generate service file
		content, err := generateSnapServicesFile(app, realBaseDir, aaProfile)
		if err != nil {
			return err
		}
		serviceFilename := generateServiceFileName(app)
		os.MkdirAll(filepath.Dir(serviceFilename), 0755)
		if err := osutil.AtomicWriteFile(serviceFilename, []byte(content), 0644, 0); err != nil {
			return err
		}
		// Generate systemd socket file if needed
		if app.Socket {
			content, err := generateSnapSocketFile(app, realBaseDir, aaProfile)
			if err != nil {
				return err
			}
			socketFilename := generateSocketFileName(app)
			os.MkdirAll(filepath.Dir(socketFilename), 0755)
			if err := osutil.AtomicWriteFile(socketFilename, []byte(content), 0644, 0); err != nil {
				return err
			}
		}
		// daemon-reload and start only if we are not in the
		// inhibitHooks mode
		//
		// *but* always run enable (which just sets a symlink)
		serviceName := filepath.Base(generateServiceFileName(app))
		sysd := systemd.New(dirs.GlobalRootDir, inter)

		if err := sysd.DaemonReload(); err != nil {
			return err
		}

		// we always enable the service even in inhibit hooks
		if err := sysd.Enable(serviceName); err != nil {
			return err
		}

		if err := sysd.Start(serviceName); err != nil {
			return err
		}

		if app.Socket {
			socketName := filepath.Base(generateSocketFileName(app))
			// we always enable the socket even in inhibit hooks
			if err := sysd.Enable(socketName); err != nil {
				return err
			}

			if err := sysd.Start(socketName); err != nil {
				return err
			}
		}
	}

	return nil
}

func removePackageServices(s *snap.Info, inter interacter) error {
	sysd := systemd.New(dirs.GlobalRootDir, inter)

	nservices := 0

	for _, app := range s.Apps {
		if app.Daemon == "" {
			continue
		}
		nservices++

		serviceName := filepath.Base(generateServiceFileName(app))
		if err := sysd.Disable(serviceName); err != nil {
			return err
		}
		if err := sysd.Stop(serviceName, serviceStopTimeout(app)); err != nil {
			if !systemd.IsTimeout(err) {
				return err
			}
			inter.Notify(fmt.Sprintf("%s refused to stop, killing.", serviceName))
			// ignore errors for kill; nothing we'd do differently at this point
			sysd.Kill(serviceName, "TERM")
			time.Sleep(killWait)
			sysd.Kill(serviceName, "KILL")
		}

		if err := os.Remove(generateServiceFileName(app)); err != nil && !os.IsNotExist(err) {
			logger.Noticef("Failed to remove service file for %q: %v", serviceName, err)
		}

		if err := os.Remove(generateSocketFileName(app)); err != nil && !os.IsNotExist(err) {
			logger.Noticef("Failed to remove socket file for %q: %v", serviceName, err)
		}

		// XXX where/when is this generated? genBusPolicyFile is never alled atm
		// Also remove DBus system policy file
		if err := os.Remove(generateBusPolicyFileName(app)); err != nil && !os.IsNotExist(err) {
			logger.Noticef("Failed to remove bus policy file for service %q: %v", serviceName, err)
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
