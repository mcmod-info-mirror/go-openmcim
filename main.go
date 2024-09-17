/**
 * OpenBmclAPI (Golang Edition)
 * Copyright (C) 2023 Kevin Z <zyxkad@gmail.com>
 * All rights reserved
 *
 *  This program is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU Affero General Public License as published
 *  by the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  This program is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *  GNU Affero General Public License for more details.
 *
 *  You should have received a copy of the GNU Affero General Public License
 *  along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"runtime/pprof"

	doh "github.com/libp2p/go-doh-resolver"

	"github.com/LiterMC/go-openbmclapi/database"
	"github.com/LiterMC/go-openbmclapi/internal/build"
	"github.com/LiterMC/go-openbmclapi/lang"
	"github.com/LiterMC/go-openbmclapi/limited"
	"github.com/LiterMC/go-openbmclapi/log"

	_ "github.com/LiterMC/go-openbmclapi/lang/en"
	_ "github.com/LiterMC/go-openbmclapi/lang/zh"
)

const ClusterServerURL = "https://files.mcimirror.top"

var (
	KeepAliveInterval = time.Second * 59
)

var startTime = time.Now()

var config Config = defaultConfig

const baseDir = "."

func parseArgs() {
	if len(os.Args) > 1 {
		subcmd := strings.ToLower(os.Args[1])
		switch subcmd {
		case "main", "serve":
			break
		case "license":
			printLongLicense()
			os.Exit(0)
		case "version", "--version":
			fmt.Printf("Go-OpenBmclApi v%s (%s)\n", build.ClusterVersion, build.BuildVersion)
			os.Exit(0)
		case "help", "--help":
			printHelp()
			os.Exit(0)
		case "zip-cache":
			cmdZipCache(os.Args[2:])
			os.Exit(0)
		case "unzip-cache":
			cmdUnzipCache(os.Args[2:])
			os.Exit(0)
		case "upload-webdav":
			cmdUploadWebdav(os.Args[2:])
			os.Exit(0)
		default:
			fmt.Println("Unknown sub command:", subcmd)
			printHelp()
			os.Exit(0x7f)
		}
	}
}

var exitCh = make(chan int, 1)

func osExit(n int) {
	select {
	case exitCh <- n:
	default:
	}
	runtime.Goexit()
}

func main() {
	if runtime.GOOS == "windows" {
		lang.SetLang("zh-cn")
	} else {
		lang.SetLang("en-us")
	}
	lang.ParseSystemLanguage()
	log.Debug("language:", lang.GetLang().Code())

	printShortLicense()
	parseArgs()

	exitCode := -1
	defer func() {
		code := exitCode
		if code == -1 {
			select {
			case code = <-exitCh:
			default:
				code = 0
			}
		}
		if code != 0 {
			log.Errorf(Tr("program.exited"), code)
			log.Error(Tr("error.exit.please.read.faq"))
			if runtime.GOOS == "windows" && !config.Advanced.DoNotOpenFAQOnWindows {
				log.Warn(Tr("warn.exit.detected.windows.open.browser"))
				cmd := exec.Command("cmd", "/C", "start", "https://cdn.crashmc.com/https://github.com/LiterMC/go-openbmclapi?tab=readme-ov-file#faq")
				cmd.Start()
				time.Sleep(time.Hour)
			}
		}
		os.Exit(code)
	}()
	defer log.RecordPanic()
	log.StartFlushLogFile()

	r := new(Runner)

START:
	ctx, cancel := context.WithCancel(context.Background())

	config = readConfig()
	if config.Advanced.DebugLog {
		log.SetLevel(log.LevelDebug)
	} else {
		log.SetLevel(log.LevelInfo)
	}
	if config.NoAccessLog {
		log.SetAccessLogSlots(-1)
	} else {
		log.SetAccessLogSlots(config.AccessLogSlots)
	}

	config.applyWebManifest(dsbManifest)

	log.Infof(Tr("program.starting"), build.ClusterVersion, build.BuildVersion)

	if config.ClusterId == defaultConfig.ClusterId || config.ClusterSecret == defaultConfig.ClusterSecret {
		log.Error(Tr("error.set.cluster.id"))
		osExit(CodeClientError)
	}

	r.InitCluster(ctx)

	go func(ctx context.Context) {
		defer log.RecordPanic()

		if !r.cluster.Connect(ctx) {
			osExit(CodeClientOrServerError)
		}

		firstSyncDone := make(chan struct{}, 0)
		go func() {
			defer log.RecordPanic()
			defer close(firstSyncDone)
			r.InitSynchronizer(ctx)
		}()

		listener := r.CreateHTTPServerListener(ctx)
		go func(listener net.Listener) {
			defer listener.Close()
			if err := r.clusterSvr.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				log.Error("Error when serving:", err)
				osExit(CodeClientError)
			}
		}(listener)

		var publicHost string
		if len(r.publicHosts) == 0 {
			publicHost = config.PublicHost
		} else {
			publicHost = r.publicHosts[0]
		}
		if !config.Tunneler.Enable {
			strPort := strconv.Itoa((int)(r.getPublicPort()))
			log.Infof(Tr("info.server.public.at"), net.JoinHostPort(publicHost, strPort), r.clusterSvr.Addr, r.getCertCount())
			if len(r.publicHosts) > 1 {
				log.Info(Tr("info.server.alternative.hosts"))
				for _, h := range r.publicHosts[1:] {
					log.Infof("\t- https://%s", net.JoinHostPort(h, strPort))
				}
			}
		}

		log.Info(Tr("info.wait.first.sync"))
		select {
		case <-firstSyncDone:
		case <-ctx.Done():
			return
		}

		r.EnableCluster(ctx)
	}(ctx)

	code := r.DoSignals(cancel)
	if r.restartFlag {
		goto START
	}
	exitCode = code
}

type Runner struct {
	restartFlag bool

	cluster    *Cluster
	clusterSvr *http.Server

	tlsConfig   *tls.Config
	listener    net.Listener
	publicHosts []string

	updating atomic.Bool
}

func (r *Runner) getPublicPort() uint16 {
	if config.PublicPort > 0 {
		return config.PublicPort
	}
	return config.Port
}

func (r *Runner) getCertCount() int {
	if r.tlsConfig == nil {
		return 0
	}
	return len(r.tlsConfig.Certificates)
}

func (r *Runner) DoSignals(cancel context.CancelFunc) int {
	signalCh := make(chan os.Signal, 1)
	log.Debugf("Receiving signals")
	signal.Notify(signalCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(signalCh)

	r.restartFlag = false
	for {
		select {
		case code := <-exitCh:
			return code
		case s := <-signalCh:
			if s == syscall.SIGQUIT {
				// avaliable commands see <https://pkg.go.dev/runtime/pprof#Profile>
				dumpCommand := "heap"
				dumpFileName := filepath.Join(os.TempDir(), fmt.Sprintf("go-openbmclapi-dump-command.%d.in", os.Getpid()))
				log.Infof("Reading dump command file at %q", dumpFileName)
				var buf [128]byte
				if dumpFile, err := os.Open(dumpFileName); err != nil {
					log.Errorf("Cannot open dump command file: %v", err)
				} else if n, err := dumpFile.Read(buf[:]); err != nil {
					dumpFile.Close()
					log.Errorf("Cannot read dump command file: %v", err)
				} else {
					dumpFile.Truncate(0)
					dumpFile.Close()
					dumpCommand = (string)(bytes.TrimSpace(buf[:n]))
				}
				pcmd := pprof.Lookup(dumpCommand)
				if pcmd == nil {
					log.Errorf("No pprof command is named %q", dumpCommand)
					continue
				}
				name := fmt.Sprintf(time.Now().Format("dump-%s-20060102-150405.txt"), dumpCommand)
				log.Infof("Creating goroutine dump file at %s", name)
				if fd, err := os.Create(name); err != nil {
					log.Infof("Cannot create dump file: %v", err)
				} else {
					err := pcmd.WriteTo(fd, 1)
					fd.Close()
					if err != nil {
						log.Infof("Cannot write dump file: %v", err)
					} else {
						log.Info("Dump file created")
					}
				}
				continue
			}

			cancel()
			shutCtx, cancelShut := context.WithTimeout(context.Background(), time.Second*15)
			log.Warn(Tr("warn.server.closing"))
			shutExit := make(chan struct{}, 0)
			go func() {
				defer close(shutExit)
				defer cancelShut()
				r.cluster.Disable(shutCtx)
				log.Warn(Tr("warn.httpserver.closing"))
				r.clusterSvr.Shutdown(shutCtx)
			}()
			select {
			case <-shutExit:
			case s := <-signalCh:
				log.Warn("signal:", s)
				log.Error("Second close signal received, exit")
				return CodeClientError
			}
			log.Warn(Tr("warn.server.closed"))
			if s == syscall.SIGHUP {
				log.Info("Restarting server ...")
				r.restartFlag = true
				return 0
			}
		}
		return 0
	}
}

func (r *Runner) InitCluster(ctx context.Context) {
	var (
		dialer *net.Dialer
		cache  = config.Cache.newCache()
	)

	_ = doh.NewResolver // TODO: use doh resolver

	r.cluster = NewCluster(ctx,
		ClusterServerURL,
		baseDir,
		config.PublicHost, r.getPublicPort(),
		config.ClusterId, config.ClusterSecret,
		config.Byoc, dialer,
		config.Storages,
		cache,
	)
	if err := r.cluster.Init(ctx); err != nil {
		log.Errorf(Tr("error.init.failed"), err)
		osExit(CodeClientError)
	}

	r.clusterSvr = &http.Server{
		Addr:        fmt.Sprintf("%s:%d", "0.0.0.0", config.Port),
		ReadTimeout: 10 * time.Second,
		IdleTimeout: 5 * time.Second,
		Handler:     r.cluster.GetHandler(),
		ErrorLog:    log.ProxiedStdLog,
	}
}

func (r *Runner) UpdateFileRecords(files []FileInfo, oldfileset map[string]int64) {
	if !config.Hijack.Enable {
		return
	}
	if !r.updating.CompareAndSwap(false, true) {
		return
	}
	defer r.updating.Store(false)

	sem := limited.NewSemaphore(12)
	log.Info("Begin to update file records")
	for _, f := range files {
		if strings.HasPrefix(f.Path, "/openbmclapi/download/") {
			continue
		}
		if oldfileset[f.Hash] > 0 {
			continue
		}
		sem.Acquire()
		go func(rec database.FileRecord) {
			defer sem.Release()
			r.cluster.database.SetFileRecord(rec)
		}(database.FileRecord{
			Path: f.Path,
			Hash: f.Hash,
			Size: f.Size,
		})
	}
	sem.Wait()
	log.Info("All file records are updated")
}

func (r *Runner) InitSynchronizer(ctx context.Context) {
	log.Info(Tr("info.filelist.fetching"))
	fl, err := r.cluster.GetFileList(ctx, 0)
	if err != nil {
		log.Errorf(Tr("error.filelist.fetch.failed"), err)
		if errors.Is(err, context.Canceled) {
			return
		}
		if !config.Advanced.SkipFirstSync {
			osExit(CodeClientOrServerError)
		}
	}

	checkCount := -1
	heavyCheck := !config.Advanced.NoHeavyCheck
	heavyCheckInterval := config.Advanced.HeavyCheckInterval
	if heavyCheckInterval <= 0 {
		heavyCheck = false
	}

	if !config.Advanced.SkipFirstSync {
		if !r.cluster.SyncFiles(ctx, fl, false) {
			return
		}
		go r.UpdateFileRecords(fl, nil)

		if !config.Advanced.NoGC {
			go r.cluster.Gc()
		}
	} else if fl != nil {
		if err := r.cluster.SetFilesetByExists(ctx, fl); err != nil {
			return
		}
	}

	var lastMod int64
	for _, f := range fl {
		if f.Mtime > lastMod {
			lastMod = f.Mtime
		}
	}

	createInterval(ctx, func() {
		log.Info(Tr("info.filelist.fetching"))
		fl, err := r.cluster.GetFileList(ctx, lastMod)
		if err != nil {
			log.Errorf(Tr("error.filelist.fetch.failed"), err)
			return
		}
		if len(fl) == 0 {
			log.Infof("No file was updated since %s", time.UnixMilli(lastMod).Format(time.DateTime))
			return
		}
		for _, f := range fl {
			if f.Mtime > lastMod {
				lastMod = f.Mtime
			}
		}

		checkCount = (checkCount + 1) % heavyCheckInterval
		oldfileset := r.cluster.CloneFileset()
		if r.cluster.SyncFiles(ctx, fl, heavyCheck && checkCount == 0) {
			go r.UpdateFileRecords(fl, oldfileset)
			if !config.Advanced.NoGC && !config.OnlyGcWhenStart {
				go r.cluster.Gc()
			}
		}
	}, (time.Duration)(config.SyncInterval)*time.Minute)
}

func (r *Runner) CreateHTTPServerListener(ctx context.Context) (listener net.Listener) {
	listener, err := net.Listen("tcp", r.clusterSvr.Addr)
	if err != nil {
		log.Errorf(Tr("error.address.listen.failed"), r.clusterSvr.Addr, err)
		osExit(CodeEnvironmentError)
	}
	if config.ServeLimit.Enable {
		limted := limited.NewLimitedListener(listener, config.ServeLimit.MaxConn, 0, config.ServeLimit.UploadRate*1024)
		limted.SetMinWriteRate(1024)
		listener = limted
	}

	tlsConfig := r.GenerateTLSConfig(ctx)
	r.publicHosts = make([]string, 0, 2)
	if tlsConfig != nil {
		for _, cert := range tlsConfig.Certificates {
			if h, err := parseCertCommonName(cert.Certificate[0]); err == nil {
				r.publicHosts = append(r.publicHosts, strings.ToLower(h))
			}
		}
		listener = newHttpTLSListener(listener, tlsConfig, r.publicHosts, r.getPublicPort())
	}
	r.listener = listener
	return
}

func (r *Runner) GenerateTLSConfig(ctx context.Context) (tlsConfig *tls.Config) {
	if config.UseCert {
		if len(config.Certificates) == 0 {
			log.Error(Tr("error.cert.not.set"))
			osExit(CodeClientError)
		}
		tlsConfig = new(tls.Config)
		tlsConfig.Certificates = make([]tls.Certificate, len(config.Certificates))
		for i, c := range config.Certificates {
			var err error
			tlsConfig.Certificates[i], err = tls.LoadX509KeyPair(c.Cert, c.Key)
			if err != nil {
				log.Errorf(Tr("error.cert.parse.failed"), i, err)
				osExit(CodeClientError)
			}
		}
	}
	if !config.Byoc {
		log.Info(Tr("info.cert.requesting"))
		tctx, cancel := context.WithTimeout(ctx, time.Minute*10)
		pair, err := r.cluster.RequestCert(tctx)
		cancel()
		if err != nil {
			log.Errorf(Tr("error.cert.request.failed"), err)
			osExit(CodeServerError)
		}
		if tlsConfig == nil {
			tlsConfig = new(tls.Config)
		}
		var cert tls.Certificate
		cert, err = tls.X509KeyPair(([]byte)(pair.Cert), ([]byte)(pair.Key))
		if err != nil {
			log.Errorf(Tr("error.cert.requested.parse.failed"), err)
			osExit(CodeServerUnexpectedError)
		}
		tlsConfig.Certificates = append(tlsConfig.Certificates, cert)
		certHost, _ := parseCertCommonName(cert.Certificate[0])
		log.Infof(Tr("info.cert.requested"), certHost)
	}
	r.tlsConfig = tlsConfig
	return
}

func (r *Runner) EnableCluster(ctx context.Context) {
	if config.Advanced.WaitBeforeEnable > 0 {
		select {
		case <-time.After(time.Second * (time.Duration)(config.Advanced.WaitBeforeEnable)):
		case <-ctx.Done():
			return
		}
	}

	if config.Tunneler.Enable {
		r.enableClusterByTunnel(ctx)
	} else {
		if err := r.cluster.Enable(ctx); err != nil {
			log.Errorf(Tr("error.cluster.enable.failed"), err)
			if ctx.Err() != nil {
				return
			}
			osExit(CodeServerOrEnvionmentError)
		}
	}
}

func (r *Runner) enableClusterByTunnel(ctx context.Context) {
	cmd := exec.CommandContext(ctx, config.Tunneler.TunnelProg)
	log.Infof(Tr("info.tunnel.running"), cmd.String())
	var (
		cmdOut, cmdErr io.ReadCloser
		err            error
	)
	cmd.Env = append(os.Environ(),
		"CLUSTER_PORT="+strconv.Itoa((int)(config.Port)))
	if cmdOut, err = cmd.StdoutPipe(); err != nil {
		log.Errorf(Tr("error.tunnel.command.prepare.failed"), err)
		osExit(CodeClientUnexpectedError)
	}
	if cmdErr, err = cmd.StderrPipe(); err != nil {
		log.Errorf(Tr("error.tunnel.command.prepare.failed"), err)
		osExit(CodeClientUnexpectedError)
	}
	if err = cmd.Start(); err != nil {
		log.Errorf(Tr("error.tunnel.command.prepare.failed"), err)
		osExit(CodeClientError)
	}
	type addrOut struct {
		host string
		port uint16
	}
	detectedCh := make(chan addrOut, 1)
	onLog := func(line []byte) {
		res := config.Tunneler.outputRegex.FindSubmatch(line)
		if res == nil {
			return
		}
		tunnelHost, tunnelPort := res[config.Tunneler.hostOut], res[config.Tunneler.portOut]
		if len(tunnelHost) > 0 && tunnelHost[0] == '[' && tunnelHost[len(tunnelHost)-1] == ']' { // a IPv6 with port [<ipv6>]:<port>
			tunnelHost = tunnelHost[1 : len(tunnelHost)-1]
		}
		port, err := strconv.Atoi((string)(tunnelPort))
		if err != nil {
			log.Panic(err)
		}
		select {
		case detectedCh <- addrOut{
			host: (string)(tunnelHost),
			port: (uint16)(port),
		}:
		default:
		}
	}
	go func() {
		defer cmdOut.Close()
		defer cmd.Process.Kill()
		sc := bufio.NewScanner(cmdOut)
		for sc.Scan() {
			log.Info("[tunneler/stdout]:", sc.Text())
			onLog(sc.Bytes())
		}
	}()
	go func() {
		defer cmdErr.Close()
		defer cmd.Process.Kill()
		sc := bufio.NewScanner(cmdErr)
		for sc.Scan() {
			log.Info("[tunneler/stderr]:", sc.Text())
			onLog(sc.Bytes())
		}
	}()
	go func() {
		defer log.RecordPanic()
		defer cmd.Process.Kill()
		for {
			select {
			case addr := <-detectedCh:
				log.Infof(Tr("info.tunnel.detected"), addr.host, addr.port)
				r.cluster.publicPort = addr.port
				if !r.cluster.byoc {
					r.cluster.host = addr.host
				}
				strPort := strconv.Itoa((int)(r.getPublicPort()))
				if spp, ok := r.listener.(interface{ SetPublicPort(port string) }); ok {
					spp.SetPublicPort(strPort)
				}
				log.Infof(Tr("info.server.public.at"), net.JoinHostPort(addr.host, strPort), r.clusterSvr.Addr, r.getCertCount())
				if len(r.publicHosts) > 1 {
					log.Info(Tr("info.server.alternative.hosts"))
					for _, h := range r.publicHosts[1:] {
						log.Infof("\t- https://%s", net.JoinHostPort(h, strPort))
					}
				}
				if !r.cluster.Enabled() {
					shutCtx, cancel := context.WithTimeout(ctx, time.Minute)
					r.cluster.Disable(shutCtx)
					cancel()
				}
				if err := r.cluster.Enable(ctx); err != nil {
					log.Errorf(Tr("error.cluster.enable.failed"), err)
					if ctx.Err() != nil {
						return
					}
					osExit(CodeServerOrEnvionmentError)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	if _, err := cmd.Process.Wait(); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Errorf("Tunnel program exited: %v", err)
		osExit(CodeClientError)
	}
	// TODO: maybe restart the tunnel program?
}

func Tr(name string) string {
	return lang.Tr(name)
}
