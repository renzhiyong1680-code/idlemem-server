package scheduler

import "errors"

// ErrNoNodes 当前没有可用节点承接订单/分片。
var ErrNoNodes = errors.New("scheduler: no available nodes")
