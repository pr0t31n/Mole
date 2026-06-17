// moled — per-activation token verification (§10.3).
//
// In token-callback mode the SOCKS5 password is a Helm-issued per-activation
// JWT. moled does NOT verify the signature itself; it POSTs the opaque token to
// Helm's /api/v1/mole/verify (which checks signature + expiry + revocation +
// scope) and caches the boolean result for 60 s to bound load.
package socks5

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const verifyCacheTTL = 60 * time.Second

type verifyResult struct {
	valid    bool
	codename string
	exp      time.Time
}

var (
	verifyMu    sync.Mutex
	verifyCache = map[string]verifyResult{}
)

// verifyToken returns (valid, codename), using a 60 s cache keyed by the token
// hash. Network/decode errors fail closed (valid=false).
func (s *Server) verifyToken(token string) (bool, string) {
	key := hashToken(token)
	now := time.Now()

	verifyMu.Lock()
	if e, ok := verifyCache[key]; ok && now.Before(e.exp) {
		verifyMu.Unlock()
		return e.valid, e.codename
	}
	verifyMu.Unlock()

	valid, codename := s.callVerify(token)

	verifyMu.Lock()
	verifyCache[key] = verifyResult{valid: valid, codename: codename, exp: now.Add(verifyCacheTTL)}
	verifyMu.Unlock()
	return valid, codename
}

func (s *Server) callVerify(token string) (bool, string) {
	body, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequest(http.MethodPost, s.VerifyURL, bytes.NewReader(body))
	if err != nil {
		return false, ""
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("token verify call failed: %v", err)
		}
		return false, "" // fail closed
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false, ""
	}
	var out struct {
		Valid    bool   `json:"valid"`
		Codename string `json:"codename"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, ""
	}
	return out.Valid, out.Codename
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}
