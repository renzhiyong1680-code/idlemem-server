// 备份导出与恢复：控制面元数据的灾难重建、跨环境搬移与回归演练。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/xiancuncloud/idlemem-server/internal/backup"
)

func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	snap, err := backup.Export(s.store, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "export failed: "+err.Error())
		return
	}
	name := fmt.Sprintf("xiancun-backup-%s.json", snap.CreatedAt.Format("20060102-150405"))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	var snap backup.Snapshot
	if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	res, err := backup.Restore(s.store, &snap)
	if err != nil {
		writeError(w, http.StatusBadRequest, "restore failed: "+err.Error())
		return
	}
	s.logger.Printf("api: backup restored nodes=%d tenants=%d orders=%d shards=%d usage=%d baselines=%d invoices=%d",
		res.Nodes, res.Tenants, res.Orders, res.Shards, res.Usage, res.Baselines, res.Invoices)
	writeJSON(w, http.StatusOK, map[string]any{
		"restored": res,
		"note":     "nodes/tenants are upserted; node liveness comes back on their next heartbeat",
	})
}
