//go:build linux
// +build linux

package l4log

import "github.com/GuanceCloud/datakit-ebpf/pkg/spanid"

var _ulid *spanid.ULID

func genID128() (*spanid.ID128, bool) {
	if _ulid != nil {
		return _ulid.ID()
	}
	return nil, false
}

func initULID() {
	var err error

	_ulid, err = spanid.NewULID()
	if err != nil {
		log.Errorf("failed to init ulid: %v", err)
	}
}
