// This file implements the common Redis commands beyond SET/GET/DEL that
// client libraries expect to work: EXISTS, MGET, MSET and INCR. They keep the
// same tenant isolation and capacity gate as SET.
package gateway

import (
	"strconv"

	"github.com/tidwall/buntdb"
	"github.com/tidwall/redcon"

	"github.com/xiancuncloud/idlemem-server/internal/model"
)

// lookup 取一个键的值：先查本地缓存，未命中再回源到承载节点。
func (g *Gateway) lookup(t *model.Tenant, key string) (string, bool) {
	nsKey := namespaceKey(t, key)
	var val string
	found := false
	_ = g.cache.View(func(tx *buntdb.Tx) error {
		if v, err := tx.Get(nsKey); err == nil {
			val, found = v, true
		}
		return nil
	})
	if found {
		return val, true
	}
	nodes, err := g.onlineShardNodes(t.ID)
	if err == nil && len(nodes) > 0 {
		if v, ok := g.forwardGet(nodes, nsKey); ok {
			return string(v), true
		}
	}
	return "", false
}

// localLen 返回本地缓存中该键的长度（不存在为 0）。
func (g *Gateway) localLen(nsKey string) int {
	n := 0
	_ = g.cache.View(func(tx *buntdb.Tx) error {
		if v, err := tx.Get(nsKey); err == nil {
			n = len(v)
		}
		return nil
	})
	return n
}

func (g *Gateway) handleExists(conn redcon.Conn, args []string) {
	t := g.requireTenant(conn)
	if t == nil {
		return
	}
	if len(args) < 2 {
		conn.WriteError("ERR wrong number of arguments for 'exists' command")
		return
	}
	n := 0
	for _, k := range args[1:] {
		if _, ok := g.lookup(t, k); ok {
			n++
		}
	}
	conn.WriteInt(n)
}

func (g *Gateway) handleMGet(conn redcon.Conn, args []string) {
	t := g.requireTenant(conn)
	if t == nil {
		return
	}
	if len(args) < 2 {
		conn.WriteError("ERR wrong number of arguments for 'mget' command")
		return
	}
	conn.WriteArray(len(args) - 1)
	for _, k := range args[1:] {
		if v, ok := g.lookup(t, k); ok {
			conn.WriteBulkString(v)
		} else {
			conn.WriteNull()
		}
	}
}

func (g *Gateway) handleMSet(conn redcon.Conn, args []string) {
	t := g.requireTenant(conn)
	if t == nil {
		return
	}
	// MSET key value [key value ...]
	if len(args) < 3 || len(args)%2 == 0 {
		conn.WriteError("ERR wrong number of arguments for 'mset' command")
		return
	}

	nsKeys := make([]string, 0, (len(args)-1)/2)
	values := make([]string, 0, (len(args)-1)/2)
	deltas := make([]int64, 0, (len(args)-1)/2)
	for i := 1; i+1 < len(args); i += 2 {
		nsKey := namespaceKey(t, args[i])
		old := g.localLen(nsKey)
		nsKeys = append(nsKeys, nsKey)
		values = append(values, args[i+1])
		deltas = append(deltas, int64(len(args[i+1])-old))
	}

	delta, err := g.checkBatch(t, deltas)
	if err != nil {
		conn.WriteError("ERR " + err.Error())
		return
	}

	err = g.cache.Update(func(tx *buntdb.Tx) error {
		for i, nsKey := range nsKeys {
			if _, _, err := tx.Set(nsKey, values[i], nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		conn.WriteError("ERR " + err.Error())
		return
	}
	g.commit(t.ID, delta)

	// 数据面：整批转发到承载节点（尽力而为，本地缓存已成功）。
	nodes, ferr := g.onlineShardNodes(t.ID)
	if ferr == nil && len(nodes) > 0 {
		for i, nsKey := range nsKeys {
			if _, err := g.forwardSet(nodes, nsKey, values[i], 0); err != nil {
				g.logger.Printf("gateway: forward mset %q: %v (local cache kept)", nsKey, err)
			}
		}
	}
	conn.WriteString("OK")
}

func (g *Gateway) handleIncr(conn redcon.Conn, args []string) {
	t := g.requireTenant(conn)
	if t == nil {
		return
	}
	if len(args) < 2 {
		conn.WriteError("ERR wrong number of arguments for 'incr' command")
		return
	}
	key := args[1]
	old, ok := g.lookup(t, key)
	cur := int64(0)
	if ok {
		n, err := strconv.ParseInt(old, 10, 64)
		if err != nil {
			conn.WriteError("ERR value is not an integer or out of range")
			return
		}
		cur = n
	}
	next := cur + 1
	newVal := strconv.FormatInt(next, 10)
	nsKey := namespaceKey(t, key)

	delta, err := g.checkSet(t, len(old), len(newVal))
	if err != nil {
		conn.WriteError("ERR " + err.Error())
		return
	}
	err = g.cache.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(nsKey, newVal, nil)
		return err
	})
	if err != nil {
		conn.WriteError("ERR " + err.Error())
		return
	}
	g.commit(t.ID, delta)

	nodes, ferr := g.onlineShardNodes(t.ID)
	if ferr == nil && len(nodes) > 0 {
		if _, err := g.forwardSet(nodes, nsKey, newVal, 0); err != nil {
			g.logger.Printf("gateway: forward incr %q: %v (local cache kept)", nsKey, err)
		}
	}
	conn.WriteInt64(next)
}
