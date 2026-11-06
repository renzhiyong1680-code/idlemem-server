package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// SetTokens 设置管理面与节点面上报的访问令牌。
//
// 为空表示该面不校验（仅建议本地开发/内测使用）；生产环境必须设置，
// 否则任何人都能创建租户、修改单价或停服。
func (s *Server) SetTokens(admin, node string) {
	s.adminToken = admin
	s.nodeToken = node
}

// authMiddleware 按路径选择校验的管理面/节点面令牌。
//
// 放行规则：
//   - /healthz 始终开放（探活）；
//   - 节点注册与心跳：node_token 或 admin_token 任一匹配即可；
//   - 其余接口：admin_token 匹配即可。
//
// 令牌通过 Authorization: Bearer <token> 或 X-Admin-Token / X-Node-Token 传递。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		header := r.Header.Get("Authorization")
		bearer := ""
		if strings.HasPrefix(header, "Bearer ") {
			bearer = strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		}

		isNodePath := r.URL.Path == "/api/v1/nodes/register" || r.URL.Path == "/api/v1/nodes/heartbeat"
		if isNodePath {
			nodeTok := firstNonEmpty(r.Header.Get("X-Node-Token"), bearer)
			if s.nodeToken != "" && tokenEqual(nodeTok, s.nodeToken) {
				next.ServeHTTP(w, r)
				return
			}
			adminTok := firstNonEmpty(r.Header.Get("X-Admin-Token"), bearer)
			if s.adminToken != "" && tokenEqual(adminTok, s.adminToken) {
				next.ServeHTTP(w, r)
				return
			}
			if s.nodeToken == "" && s.adminToken == "" {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized: invalid node token")
			return
		}

		if s.adminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		adminTok := firstNonEmpty(r.Header.Get("X-Admin-Token"), bearer)
		if tokenEqual(adminTok, s.adminToken) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized: invalid admin token")
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// tokenEqual 常量时间比较，避免时序侧信道。
func tokenEqual(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// GenerateToken 生成 32 字节（64 个十六进制字符）的随机令牌，用于管理面/节点面。
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
