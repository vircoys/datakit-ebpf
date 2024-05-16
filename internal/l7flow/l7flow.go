//go:build linux
// +build linux

// Package l7flow collects http(s) request flow
package l7flow

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"regexp"
	"sync"
	"time"
	"unsafe"

	manager "github.com/DataDog/ebpf-manager"
	"github.com/GuanceCloud/cliutils/logger"
	"github.com/GuanceCloud/cliutils/point"
	dkebpf "github.com/GuanceCloud/datakit-ebpf/internal/c"
	"github.com/GuanceCloud/datakit-ebpf/internal/exporter"
	"github.com/GuanceCloud/datakit-ebpf/internal/l7flow/comm"
	"github.com/GuanceCloud/datakit-ebpf/internal/l7flow/protodec"
	dknetflow "github.com/GuanceCloud/datakit-ebpf/internal/netflow"
	sysmonitor "github.com/GuanceCloud/datakit-ebpf/internal/sysmonitor"
	"github.com/GuanceCloud/datakit-ebpf/internal/tracing"
	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	expRand "golang.org/x/exp/rand"
)

// #include "../c/apiflow/l7_stats.h"
import "C"

const HTTPPayloadMaxsize = 157

// const srcNameM = "httpflow"

const (
	NoValue           = "N/A"
	DirectionOutgoing = "outgoing"
	DirectionIncoming = "incoming"
)

const (
	ConnL3Mask uint32 = dknetflow.ConnL3Mask
	ConnL3IPv4 uint32 = dknetflow.ConnL3IPv4
	ConnL3IPv6 uint32 = dknetflow.ConnL3IPv6

	ConnL4Mask uint32 = dknetflow.ConnL4Mask
	ConnL4TCP  uint32 = dknetflow.ConnL4TCP
	ConnL4UDP  uint32 = dknetflow.ConnL4UDP

	L7BufferShift     = 12
	PayloadBufSize    = 1 << L7BufferShift
	KernelTaskCommLen = 16
)

var (
	// libssl
	RegexpLibSSL    = regexp.MustCompile(`libssl.so`)
	RegexpLibCrypto = regexp.MustCompile(`libcrypto.so`)

	// TODO: guntls
)

type (
	CLayer7Http      C.struct_layer7_http
	CHTTPReqFinished C.struct_http_req_finished
	CL7Buffer        C.struct_netwrk_data

	ConnectionInfoC dknetflow.ConnectionInfoC

	HTTPStats struct {
		Direction string

		ReqMethod uint8

		Path     string
		RespCode uint32

		HTTPVersion uint32

		// PidTid uint64

		Recv int
		Send int

		ReqSeq  int64
		RespSeq int64

		ReqTS  uint64
		RespTS uint64
	}

	HTTPReqFinishedInfo struct {
		ConnInfo  comm.ConnectionInfo
		HTTPStats HTTPStats
	}
)

func readMeta(buf *CL7Buffer, dst *comm.ConnectionInfo) {
	conn := buf.meta.conn

	// TODO: record thread name
	//
	cmdB := *(*[KernelTaskCommLen]byte)(unsafe.Pointer(&buf.meta.comm))
	cmdCpy := make([]byte, 0, len(buf.meta.comm))
	cmdCpy = append(cmdCpy, cmdB[:]...)
	cmdCpy = bytes.Trim(cmdCpy, "\u0000")
	cmdCpy = bytes.TrimSpace(cmdCpy)
	taskComm := string(cmdCpy)

	dst.Saddr = (*(*[4]uint32)(unsafe.Pointer(&conn.saddr))) //nolint:gosec
	dst.Daddr = (*(*[4]uint32)(unsafe.Pointer(&conn.daddr))) //nolint:gosec
	dst.Sport = uint32(conn.sport)
	dst.Dport = uint32(conn.dport)
	dst.Pid = uint32(conn.pid)
	dst.Netns = uint32(conn.netns)
	dst.Meta = uint32(conn.meta)
	dst.TaskName = taskComm
}

var log = logger.DefaultSLogger("ebpf")

var randInnerID func() int64

func newRandFunc() func() int64 {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err == nil {
		v := binary.LittleEndian.Uint64(b)
		r := expRand.New(expRand.NewSource(v))
		r.Seed(v)
		return func() int64 {
			return r.Int63()
		}
	}
	return func() int64 {
		return -1
	}
}

func Init(nl *logger.Logger) {
	log = nl
	comm.Init(nl)
	protodec.Init()
}

var (
	libSSLSection = []string{
		"uprobe__SSL_read",
		"uretprobe__SSL_read",
		"uprobe__SSL_write",
		"uprobe__SSL_shutdown",
		"uprobe__SSL_set_fd",
		"uprobe__SSL_set_bio",
	}
	libcryptoSection = []string{
		"uprobe__BIO_new_socket",
		"uretprobe__BIO_new_socket",
	}
)

type perferEventHandle func(cpu int, data []byte, perfmap *manager.PerfMap,
	manager *manager.Manager)

func NewHTTPFlowManger(constEditor []manager.ConstantEditor, bmaps map[string]*ebpf.Map,
	bufHandler perferEventHandle, enableTLS bool) (*manager.Manager, *sysmonitor.UprobeRegister, error) {
	randInnerID = newRandFunc()

	m := &manager.Manager{
		Probes: []*manager.Probe{
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_enter_read",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_exit_read",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_enter_write",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_exit_write",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_enter_recvfrom",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_exit_recvfrom",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_enter_sendto",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_exit_sendto",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_enter_writev",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_exit_writev",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_enter_readv",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_exit_readv",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "kprobe__tcp_close",
					UID:          "tcp_close_apiflow",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_enter_sendfile64",
				},
			},
			{
				ProbeIdentificationPair: manager.ProbeIdentificationPair{
					EBPFFuncName: "tracepoint__sys_exit_sendfile64",
				},
			},
		},
		PerfMaps: []*manager.PerfMap{
			{
				Map: manager.Map{
					Name: "mp_upload_netwrk_data",
				},
				PerfMapOptions: manager.PerfMapOptions{
					// pagesize ~= 4k,
					PerfRingBufferSize: 4096 * os.Getpagesize(),
					DataHandler:        bufHandler,
					LostHandler: func(CPU int, count uint64, perfMap *manager.PerfMap, manager *manager.Manager) {
						log.Warnf("lost %d events on cpu %d\n", count, CPU)
					},
				},
			},
		},
	}

	var r *sysmonitor.UprobeRegister
	if enableTLS {
		opensslRules := []sysmonitor.UprobeRegRule{
			{
				Re:         RegexpLibSSL,
				Register:   sysmonitor.NewRegisterFunc(m, libSSLSection),
				UnRegister: sysmonitor.NewUnRegisterFunc(m, libSSLSection),
			},
			{
				Re:         RegexpLibCrypto,
				Register:   sysmonitor.NewRegisterFunc(m, libcryptoSection),
				UnRegister: sysmonitor.NewUnRegisterFunc(m, libcryptoSection),
			},
		}

		var err error
		r, err = sysmonitor.NewUprobeDyncLibRegister(opensslRules)
		if err != nil {
			return nil, nil, err
		}
	}

	mOpts := manager.Options{
		RLimit: &unix.Rlimit{
			Cur: math.MaxUint64,
			Max: math.MaxUint64,
		},
		ConstantEditors: constEditor,
	}

	if bmaps != nil {
		mOpts.MapEditors = bmaps
	}

	if buf, err := dkebpf.APIFlowBin(); err != nil {
		return nil, nil, fmt.Errorf("apiflow.o: %w", err)
	} else if err := m.InitWithOptions((bytes.NewReader(buf)), mOpts); err != nil {
		return nil, nil, err
	}

	return m, r, nil
}

type HTTPFlowTracer struct {
	gTags          map[string]string
	datakitPostURL string
	tracePostURL   string
	conv2dd        bool
	enableTrace    bool
	procFilter     *tracing.ProcessFilter

	tracer *Tracer
}

var _datakitPostURL string
var _tracePostURL string
var _enableTrace bool

var selfPid = 0

func NewHTTPFlowTracer(ctx context.Context, tags map[string]string, datakitPostURL, tracePostURL string,
	conv2dd, enableTrace bool, filter *tracing.ProcessFilter) *HTTPFlowTracer {
	_tracePostURL = tracePostURL
	_datakitPostURL = datakitPostURL
	_enableTrace = enableTrace

	if selfPid == 0 {
		selfPid = os.Getpid()
	}
	return &HTTPFlowTracer{
		gTags:          tags,
		datakitPostURL: datakitPostURL,
		tracePostURL:   tracePostURL,
		conv2dd:        conv2dd,
		enableTrace:    enableTrace,
		procFilter:     filter,
		tracer: newTracer(ctx, filter, tags, k8sNetInfo,
			_datakitPostURL, _tracePostURL, enableTrace),
	}
}

func (tracer *HTTPFlowTracer) Run(ctx context.Context, constEditor []manager.ConstantEditor,
	bmaps map[string]*ebpf.Map, enableTLS bool, interval time.Duration) error {
	if selfPid == 0 {
		selfPid = os.Getpid()
	}
	go tracer.tracer.Start(ctx, interval)

	bpfManger, r, err := NewHTTPFlowManger(constEditor, bmaps,
		tracer.tracer.PerfEventHandle, enableTLS)
	if err != nil {
		return err
	}

	if err := bpfManger.Start(); err != nil {
		log.Error(err)
		return err
	}

	log.Info("api tracer starting ...")

	if r != nil {
		r.ScanAndUpdate()
		r.Monitor(ctx, time.Second*30)
	}

	go func() {
		<-ctx.Done()
		_ = bpfManger.Stop(manager.CleanAll)
	}()

	return nil
}

var netwrksyncPool = sync.Pool{
	New: func() interface{} {
		return &comm.NetwrkData{
			Payload: make([]byte, 0, PayloadBufSize),
		}
	},
}

func getNetwrkData() *comm.NetwrkData {
	return netwrksyncPool.Get().(*comm.NetwrkData)
}

func putNetwrkData(data *comm.NetwrkData) {
	if data == nil {
		return
	}
	data.Conn = comm.ConnectionInfo{}
	data.ConnUniID = comm.ConnUniID{}

	data.ActSize = 0
	data.TCPSeq = 0
	data.Thread[0] = 0
	data.Thread[1] = 0
	data.TS = 0
	data.Fn = 0
	data.TSTail = 0
	data.Index = 0
	data.Payload = data.Payload[:0]

	netwrksyncPool.Put(data)
}

func feed(url string, data []*point.Point, gzip bool) error {
	if len(data) == 0 {
		return nil
	}
	if err := exporter.FeedPoint(url, data, gzip); err != nil {
		return err
	}
	return nil
}
