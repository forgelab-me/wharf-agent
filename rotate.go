package main

// Certificate rotation, cf. ARCHITECTURE.md "Enrôlement d'un nouvel agent".
// Two-phase so that no crash can lock the agent out:
//
//  1. a new key pair is generated next to the current one (never replacing
//     it yet) and its *public* certificate is announced to the controller,
//     authenticated by the current certificate -- the new key never travels;
//  2. only once the controller acknowledged does the agent install it. The
//     controller accepts both fingerprints until the new one is first used,
//     then drops the old one.
//
// If the agent dies anywhere in between, the next attempt reuses the
// prepared pair and announces the same fingerprint again (idempotent).

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/forgelab-me/wharf-agent/internal/identity"
)

var (
	certMu      sync.RWMutex
	currentCert tls.Certificate

	lastRotateAttempt time.Time
)

func setClientCert(c tls.Certificate) {
	certMu.Lock()
	currentCert = c
	certMu.Unlock()
}

func getClientCert(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	certMu.RLock()
	defer certMu.RUnlock()
	c := currentCert
	return &c, nil
}

// maybeRotate is cheap enough to call on every poll tick: it returns at
// once unless the current certificate is older than `after`.
func maybeRotate(client *http.Client, controllerURL, identityDir string, after time.Duration) {
	if after <= 0 {
		return
	}
	certMu.RLock()
	cur := currentCert
	certMu.RUnlock()
	if identity.Age(cur) < after {
		return
	}
	// A failed attempt (controller refusing, network down) is retried at
	// most every 10 minutes instead of on every 10s poll tick.
	if time.Since(lastRotateAttempt) < 10*time.Minute {
		return
	}
	lastRotateAttempt = time.Now()

	next, err := identity.PrepareNext(identityDir)
	if err != nil {
		log.Println("rotation: prepare new identity:", err)
		return
	}
	body, err := json.Marshal(struct {
		CertPEM string `json:"cert_pem"`
	}{CertPEM: string(identity.PEM(next))})
	if err != nil {
		log.Println("rotation: marshal:", err)
		return
	}
	resp, err := client.Post(controllerURL+"/agent/rotate", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Println("rotation: announce failed, will retry:", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("rotation: controller refused (%d): %s", resp.StatusCode, bytes.TrimSpace(b))
		return
	}

	installed, err := identity.CommitNext(identityDir)
	if err != nil {
		log.Println("rotation: install failed, will retry:", err)
		return
	}
	setClientCert(installed)
	client.CloseIdleConnections()
	log.Println("rotation: now using certificate", identity.Fingerprint(installed.Certificate[0]))
}
