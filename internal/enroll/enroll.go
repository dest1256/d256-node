// Package enroll performs the one-time enrollment against the panel.
package enroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dest1256/d256-node/internal/state"
	"github.com/dest1256/d256-node/internal/version"
)

const maxResponse = 64 << 10

type request struct {
	Token       string `json:"token"`
	CSR         string `json:"csr"`
	NodeVersion string `json:"nodeVersion"`
}

type response struct {
	NodeID   int    `json:"nodeId"`
	Cert     string `json:"cert"`
	CA       string `json:"ca"`
	GRPCAddr string `json:"grpcAddr"`
	Error    string `json:"error"`
}

// Options tune Enroll.
type Options struct {
	// AllowInsecureHTTP permits an http:// panel URL (development only).
	AllowInsecureHTTP bool
	// HTTPClient overrides the default client (tests).
	HTTPClient *http.Client
}

// Enroll generates a key pair locally, redeems the token and returns the identity to store.
// The private key never leaves this process.
func Enroll(ctx context.Context, panelURL, token string, opt Options) (*state.Identity, error) {
	u, err := url.Parse(strings.TrimRight(panelURL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid panel URL %q (want https://host[/base-path])", panelURL)
	}
	if u.Scheme == "http" && !opt.AllowInsecureHTTP {
		return nil, errors.New("refusing plain http: the enrollment token would travel unencrypted (use https, or --allow-insecure-http for development)")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("enrollment token is empty")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(request{
		Token:       token,
		CSR:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		NodeVersion: version.Version,
	})
	if err != nil {
		return nil, err
	}

	endpoint := u.String() + "/d256/enroll"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := opt.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact panel: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return nil, err
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("panel answered %s with an unexpected body", res.Status)
	}
	if res.StatusCode != http.StatusOK {
		msg := out.Error
		if msg == "" {
			msg = res.Status
		}
		return nil, fmt.Errorf("enrollment refused: %s", msg)
	}
	if out.NodeID <= 0 || out.Cert == "" || out.CA == "" || out.GRPCAddr == "" {
		return nil, errors.New("panel returned an incomplete enrollment response")
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &state.Identity{
		Info: state.Info{
			NodeID: out.NodeID, PanelURL: u.String(), GRPCAddr: out.GRPCAddr, EnrolledAt: time.Now().Unix(),
		},
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		CertPEM: []byte(out.Cert),
		CAPEM:   []byte(out.CA),
	}, nil
}
