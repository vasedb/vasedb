// Copyright 2022 Leon Ding <ding@ibyte.me> https://wiredkv.github.io

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// wiredb code logic of the main process
package cmd

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/auula/wiredkv/clog"
	"github.com/auula/wiredkv/conf"
	"github.com/auula/wiredkv/server"
	"github.com/auula/wiredkv/utils"
	"github.com/auula/wiredkv/vfs"
	"github.com/gookit/color"
)

const (
	version = "v0.1.1"
	website = "https://wiredb.github.io"
)

var (
	//go:embed banner.txt
	logo   string
	banner = fmt.Sprintf(logo, version, website)
	daemon = false
)

// Initialize components needed globally,
// Parse command line input arguments,
// command line parameters have the highest priority,
// but they can set relatively fewer parameters.
func init() {
	color.RGB(255, 123, 34).Println(banner)
	fl := parseFlags()

	if conf.HasCustom(fl.config) {
		err := conf.Load(fl.config, conf.Settings)
		if err != nil {
			clog.Failed(err)
		}
		clog.Info("Loading custom config file was successfully")
	}

	if fl.debug {
		conf.Settings.Debug, clog.IsDebug = fl.debug, fl.debug
	}

	// Command line password has the highest priority
	if fl.auth != conf.Default.Password {
		conf.Settings.Password = fl.auth
	} else {
		// If no password is passed from the command line,
		// the system randomly generates a 26-character password
		conf.Settings.Password = utils.RandomString(26)
		auth := color.Yellow.Sprintf("%s", conf.Settings.Password)
		clog.Warnf("The default password is: %s", auth)
	}

	if fl.path != conf.Default.Path {
		conf.Settings.Path = fl.path
	}

	if fl.port != conf.Default.Port {
		conf.Settings.Port = fl.port
	}

	clog.Debug(conf.Settings)

	// Validate the input parameters, even if there is a default configuration,
	// the command line parameters are not constrained
	err := conf.Vaildated(conf.Settings)
	if err != nil {
		clog.Failed(err)
	}

	clog.SetOutput(conf.Settings.LogPath)
	clog.Info("Logging output initialized successfully")
}

func StartApp() {
	if daemon {
		runAsDaemon()
	} else {
		runServer()
	}
}

func runAsDaemon() {
	args := utils.SplitArgs(utils.TrimDaemon(os.Args))
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = os.Environ()

	err := cmd.Start()
	if err != nil {
		clog.Failed(err)
	}

	clog.Infof("Daemon launched PID: %d", cmd.Process.Pid)
	os.Exit(0)
}

func runServer() {
	hts, err := server.New(&server.Options{
		Port: conf.Settings.Port,
		Auth: conf.Settings.Password,
	})
	if err != nil {
		clog.Failed(err)
	}

	fss, err := vfs.OpenFS(&vfs.Options{
		FsPerm:    conf.FsPerm,
		Path:      conf.Settings.Path,
		Threshold: conf.Settings.Region.Threshold,
	})
	if err != nil {
		clog.Failed(err)
	}

	if conf.Settings.IsCompressionEnabled() {
		// Set file data to use Snappy compression algorithm
		fss.SetCompressor(vfs.SnappyCompressor)
		clog.Info("Snappy compression activated successfully")
	}

	if conf.Settings.IsEncryptionEnabled() {
		// Set file data to use AES cryptor algorithm
		fss.SetEncryptor(vfs.AESCryptor, conf.Settings.Secret())
		clog.Info("Static encryptor activated was successfully")
	}

	if conf.Settings.IsRegionGCEnabled() {
		fss.StartRegionGC(conf.Settings.RegionGCInterval())
		clog.Info("Region compression activated successfully")
	}

	if len(conf.Settings.AllowIP) > 0 {
		hts.SetAllowIP(conf.Settings.AllowIP)
		clog.Info("Setting whitelist IP successfully")
	}

	hts.SetupFS(fss)
	clog.Info("File system setup completed successfully")

	go func() {
		err := hts.Startup()
		if err != nil {
			clog.Failed(err)
		}
	}()

	// Delay output of normal messages
	time.Sleep(500 * time.Millisecond)
	clog.Infof("HTTP server started at http://%s:%d 🚀", hts.IPv4(), hts.Port())

	// Keep the daemon process alive
	blocking := make(chan os.Signal, 1)
	signal.Notify(blocking, syscall.SIGINT, syscall.SIGTERM)

	// Blocking daemon process
	<-blocking

	// Graceful exit from the program process
	err = hts.Shutdown()
	if err != nil {
		clog.Failed(err)
	}
	os.Exit(0)
}

type flags struct {
	auth   string
	port   int
	path   string
	config string
	debug  bool
}

func parseFlags() (fl *flags) {
	fl = new(flags)
	flag.StringVar(&fl.auth, "auth", conf.Default.Password, "--auth the server authentication password.")
	flag.StringVar(&fl.path, "path", conf.Default.Path, "--path the data storage directory.")
	flag.BoolVar(&fl.debug, "debug", conf.Default.Debug, "--debug enable debug mode.")
	flag.StringVar(&fl.config, "config", "", "--config the configuration file path.")
	flag.IntVar(&fl.port, "port", conf.Default.Port, "--port the HTTP server port.")
	flag.BoolVar(&daemon, "daemon", false, "--daemon run with a daemon.")
	flag.Parse()
	return
}
