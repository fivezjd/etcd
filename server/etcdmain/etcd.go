// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package etcdmain

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"go.etcd.io/etcd/client/pkg/v3/fileutil"
	"go.etcd.io/etcd/client/pkg/v3/logutil"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/pkg/v3/osutil"
	"go.etcd.io/etcd/server/v3/embed"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v2discovery"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type dirType string

var (
	dirMember = dirType("member")
	dirProxy  = dirType("proxy")
	dirEmpty  = dirType("empty")
)

func startEtcdOrProxyV2(args []string) {
	grpc.EnableTracing = false

	// 初始化配置对象
	cfg := newConfig()
	// cfg元素非常多，有日志、读写锁等等
	defaultInitialCluster := cfg.ec.InitialCluster

	// 解析命令行输入的参数
	err := cfg.parse(args[1:])
	// 使用zaplog 记录日志
	lg := cfg.ec.GetLogger()
	// If we failed to parse the whole configuration, print the error using
	// preferably the resolved logger from the config,
	// but if does not exists, create a new temporary logger.
	if lg == nil {
		var zapError error
		// use this logger
		lg, zapError = logutil.CreateDefaultZapLogger(zap.InfoLevel)
		if zapError != nil {
			fmt.Printf("error creating zap logger %v", zapError)
			os.Exit(1)
		}
	}
	//记录启动日志
	lg.Info("Running: ", zap.Strings("args", args))
	//os.Exit(0)
	//参数解析失败，参数格式不正确
	if err != nil {
		lg.Warn("failed to verify flags", zap.Error(err))
		switch err {
		case embed.ErrUnsetAdvertiseClientURLsFlag:
			//--advertise-client-urls is required when --listen-client-urls is set explicitly
			lg.Warn("advertise client URLs are not set", zap.Error(err))
		}
		os.Exit(1)
	}

	// 启动日志
	// --判断debug
	// --判断trace是否开启
	cfg.ec.SetupGlobalLoggers()

	defer func() {
		logger := cfg.ec.GetLogger()
		if logger != nil {
			// 刷新日志buffer
			logger.Sync()
		}
	}()

	defaultHost, dhErr := (&cfg.ec).UpdateDefaultClusterFromName(defaultInitialCluster)
	if defaultHost != "" {
		lg.Info(
			"detected default host for advertise",
			zap.String("host", defaultHost),
		)
	}
	if dhErr != nil {
		lg.Info("failed to detect default host", zap.Error(dhErr))
	}

	if cfg.ec.Dir == "" {
		cfg.ec.Dir = fmt.Sprintf("%v.etcd", cfg.ec.Name)
		lg.Warn(
			"'data-dir' was empty; using default",
			zap.String("data-dir", cfg.ec.Dir),
		)
	}

	// 停止channel
	var stopped <-chan struct{}
	var errc <-chan error

	//TODO 看看下面这个方法的具体意思
	which := identifyDataDirOrDie(cfg.ec.GetLogger(), cfg.ec.Dir)
	if which != dirEmpty {
		lg.Info(
			"server has already been initialized",
			zap.String("data-dir", cfg.ec.Dir),
			zap.String("dir-type", string(which)),
		)
		switch which {
		case dirMember:
			stopped, errc, err = startEtcd(&cfg.ec)
		case dirProxy:
			// v2 http 不支持
			lg.Panic("v2 http proxy has already been deprecated in 3.6", zap.String("dir-type", string(which)))
		default:
			lg.Panic(
				"unknown directory type",
				zap.String("dir-type", string(which)),
			)
		}
	} else {
		//启动etcd 入口
		stopped, errc, err = startEtcd(&cfg.ec)
		if err != nil {
			lg.Warn("failed to start etcd", zap.Error(err))
		}
	}

	//启动错误的信息，这里比较重要 TODO
	if err != nil {
		if derr, ok := err.(*errors.DiscoveryError); ok {
			switch derr.Err {
			case v2discovery.ErrDuplicateID:
				lg.Warn(
					"member has been registered with discovery service",
					zap.String("name", cfg.ec.Name),
					zap.String("discovery-token", cfg.ec.Durl),
					zap.Error(derr.Err),
				)
				lg.Warn(
					"but could not find valid cluster configuration",
					zap.String("data-dir", cfg.ec.Dir),
				)
				lg.Warn("check data dir if previous bootstrap succeeded")
				lg.Warn("or use a new discovery token if previous bootstrap failed")

			case v2discovery.ErrDuplicateName:
				lg.Warn(
					"member with duplicated name has already been registered",
					zap.String("discovery-token", cfg.ec.Durl),
					zap.Error(derr.Err),
				)
				lg.Warn("cURL the discovery token URL for details")
				lg.Warn("do not reuse discovery token; generate a new one to bootstrap a cluster")

			default:
				lg.Warn(
					"failed to bootstrap; discovery token was already used",
					zap.String("discovery-token", cfg.ec.Durl),
					zap.Error(err),
				)
				lg.Warn("do not reuse discovery token; generate a new one to bootstrap a cluster")
			}
			os.Exit(1)
		}

		if strings.Contains(err.Error(), "include") && strings.Contains(err.Error(), "--initial-cluster") {
			lg.Warn("failed to start", zap.Error(err))
			if cfg.ec.InitialCluster == cfg.ec.InitialClusterFromName(cfg.ec.Name) {
				lg.Warn("forgot to set --initial-cluster?")
			}
			if types.URLs(cfg.ec.APUrls).String() == embed.DefaultInitialAdvertisePeerURLs {
				lg.Warn("forgot to set --initial-advertise-peer-urls?")
			}
			if cfg.ec.InitialCluster == cfg.ec.InitialClusterFromName(cfg.ec.Name) && len(cfg.ec.Durl) == 0 && len(cfg.ec.DiscoveryCfg.Endpoints) == 0 {
				lg.Warn("V2 discovery settings (i.e., --discovery) or v3 discovery settings (i.e., --discovery-token, --discovery-endpoints) are not set")
			}
			os.Exit(1)
		}
		lg.Fatal("discovery failed", zap.Error(err))
	}

	osutil.HandleInterrupts(lg)

	// At this point, the initialization of etcd is done.
	// The listeners are listening on the TCP ports and ready
	// for accepting connections. The etcd instance should be
	// joined with the cluster and ready to serve incoming
	// connections.
	notifySystemd(lg) // 信号处理

	select {
	case lerr := <-errc:
		// fatal out on listener errors
		lg.Fatal("listener failed", zap.Error(lerr))
	case <-stopped:
	}

	osutil.Exit(0)
}

// startEtcd runs StartEtcd in addition to hooks needed for standalone etcd.
func startEtcd(cfg *embed.Config) (<-chan struct{}, <-chan error, error) {
	// 启动etcd
	e, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, nil, err
	}
	// 注册中断句柄，将e.Close 加入切片中（切片类型是函数回调）
	osutil.RegisterInterruptHandler(e.Close)
	select {
	case <-e.Server.ReadyNotify(): // wait for e.Server to join the cluster
	case <-e.Server.StopNotify(): // publish aborted from 'ErrStopped'
	}
	return e.Server.StopNotify(), e.Err(), nil
}

// identifyDataDirOrDie returns the type of the data dir.
// Dies if the datadir is invalid.
func identifyDataDirOrDie(lg *zap.Logger, dir string) dirType {
	names, err := fileutil.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return dirEmpty
		}
		lg.Fatal("failed to list data directory", zap.String("dir", dir), zap.Error(err))
	}

	var m, p bool
	for _, name := range names {
		switch dirType(name) {
		case dirMember:
			m = true
		case dirProxy:
			p = true
		default:
			lg.Warn(
				"found invalid file under data directory",
				zap.String("filename", name),
				zap.String("data-dir", dir),
			)
		}
	}

	if m && p {
		lg.Fatal("invalid datadir; both member and proxy directories exist")
	}
	if m {
		return dirMember
	}
	if p {
		return dirProxy
	}
	return dirEmpty
}

func checkSupportArch() {
	lg, err := logutil.CreateDefaultZapLogger(zap.InfoLevel)
	if err != nil {
		panic(err)
	}
	// to add a new platform, check https://github.com/etcd-io/website/blob/main/content/en/docs/next/op-guide/supported-platform.md
	if runtime.GOARCH == "amd64" ||
		runtime.GOARCH == "arm64" ||
		runtime.GOARCH == "ppc64le" ||
		runtime.GOARCH == "s390x" {
		return
	}
	// unsupported arch only configured via environment variable
	// so unset here to not parse through flag
	defer os.Unsetenv("ETCD_UNSUPPORTED_ARCH")
	if env, ok := os.LookupEnv("ETCD_UNSUPPORTED_ARCH"); ok && env == runtime.GOARCH {
		lg.Info("running etcd on unsupported architecture since ETCD_UNSUPPORTED_ARCH is set", zap.String("arch", env))
		return
	}

	lg.Error("running etcd on unsupported architecture since ETCD_UNSUPPORTED_ARCH is set", zap.String("arch", runtime.GOARCH))
	os.Exit(1)
}
