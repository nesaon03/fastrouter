package oauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
}

func parseSA(s string) (*serviceAccount, *rsa.PrivateKey, error) {
	var sa serviceAccount
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &sa) != nil || sa.Type != "service_account" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, nil, errors.New("paste the whole service-account JSON key file (type \"service_account\")")
	}
	blk, _ := pem.Decode([]byte(strings.ReplaceAll(sa.PrivateKey, `\n`, "\n")))
	if blk == nil {
		return nil, nil, errors.New("the key file's private_key is not valid PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		if k2, err2 := x509.ParsePKCS1PrivateKey(blk.Bytes); err2 == nil {
			return &sa, k2, nil
		}
		return nil, nil, errors.New("the key file's private_key could not be read")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("the key file's private_key is not RSA")
	}
	return &sa, rk, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// mintVertex trades a service-account JWT for a one-hour access token.
func mintVertex(ctx context.Context, e Env, saJSON string) (*Result, *serviceAccount, error) {
	sa, key, err := parseSA(saJSON)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().Unix()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": sa.ClientEmail, "scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud": googleTokenURL, "iat": now, "exp": now + 3600})
	unsigned := b64url(hdr) + "." + b64url(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return nil, nil, err
	}
	var t tokenResp
	if _, err := postForm(ctx, e.Client, googleTokenURL, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion": {unsigned + "." + b64url(sig)}}, nil, &t); err != nil {
		return nil, nil, err
	}
	if t.ExpiresIn == 0 {
		t.ExpiresIn = 3600
	}
	return &Result{Secrets: map[string]string{"accessToken": t.AccessToken}, ExpiresIn: t.ExpiresIn}, sa, nil
}

func init() {
	register(&Spec{ID: "vertex", Kind: KindPaste, RefreshLead: 5 * time.Minute,
		Paste: func(ctx context.Context, e Env, text string) (*Result, error) {
			r, sa, err := mintVertex(ctx, e, text)
			if err != nil {
				return nil, err
			}
			r.Name, r.Email = sa.ClientEmail, sa.ClientEmail
			r.Secrets["serviceAccount"] = strings.TrimSpace(text)
			r.PSD = map[string]any{"projectId": sa.ProjectID, "location": "global"}
			return r, nil
		},
		Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
			sa := firstNonEmpty(Secret(c, "serviceAccount"), Secret(c, "apiKey"))
			if sa == "" {
				return nil, ErrReauth
			}
			r, _, err := mintVertex(ctx, e, sa)
			if err != nil {
				return nil, refreshErr(err)
			}
			return r, nil
		},
	})
}
