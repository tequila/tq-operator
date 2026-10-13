package supplychain

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitorus/timestamp"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// testRegistry is an in-memory registry over TLS. With a password set it speaks the docker
// token protocol: a request without the bearer is challenged to /token, which takes the
// operator's Basic credential and answers the bearer.
type testRegistry struct {
	*httptest.Server
	repo name.Repository
	tr   http.RoundTripper

	mu       sync.Mutex
	password string
	bearer   string
	requests int
}

func newTestRegistry(t *testing.T) *testRegistry {
	t.Helper()
	r := &testRegistry{}
	h := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	r.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		password, bearer := r.password, r.bearer
		r.requests++
		r.mu.Unlock()
		if password == "" {
			h.ServeHTTP(w, req)
			return
		}
		challenge := fmt.Sprintf(`Bearer realm="%s/token",service="test-registry"`, r.URL)
		if req.URL.Path == "/token" {
			if u, p, ok := req.BasicAuth(); ok && u == RegistryUser && p == password {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"token":%q}`, bearer)
				return
			}
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"not for you"}]}`, http.StatusUnauthorized)
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+bearer {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`, http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, req)
	}))
	r.Config.ErrorLog = log.New(io.Discard, "", 0) // a client's keep-alive closing after the test is not news
	r.StartTLS()
	t.Cleanup(r.Close)
	r.tr = r.Client().Transport
	repo, err := name.NewRepository(strings.TrimPrefix(r.URL, "https://")+"/platform/operator", name.StrictValidation)
	if err != nil {
		t.Fatal(err)
	}
	r.repo = repo
	return r
}

func (r *testRegistry) requirePassword(password string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.password, r.bearer = password, "bearer-"+password
}

func (r *testRegistry) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

func (r *testRegistry) opts() []remote.Option {
	return []remote.Option{remote.WithTransport(r.tr), remote.WithContext(context.Background())}
}

// pushImage pushes a random image under tag and returns its reference by digest.
func (r *testRegistry) pushImage(t *testing.T, tag string) name.Digest {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r.repo.Tag(tag), img, r.opts()...); err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return r.repo.Digest(d.String())
}

// signature is one signature layer as cosign writes it.
type signature struct {
	payload     []byte
	annotations map[string]string
}

// sign produces the simple-signing payload over digest and its ECDSA signature by priv;
// stamp, when set, adds the RFC 3161 annotation.
func sign(t *testing.T, repo name.Repository, digest string, priv *ecdsa.PrivateKey, stamp func(sig []byte) string) signature {
	t.Helper()
	payload := fmt.Appendf(nil, `{"critical":{"identity":{"docker-reference":%q},"image":{"docker-manifest-digest":%q},"type":%q},"optional":null}`,
		repo.String(), digest, PayloadType)
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	s := signature{payload: payload, annotations: map[string]string{SignatureAnnotation: base64.StdEncoding.EncodeToString(sig)}}
	if stamp != nil {
		s.annotations[TimestampAnnotation] = stamp(sig)
	}
	return s
}

// pushSignatures writes the sha256-<digest>.sig manifest with one layer per signature.
func (r *testRegistry) pushSignatures(t *testing.T, digest name.Digest, sigs ...signature) {
	t.Helper()
	img := mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON)
	for _, s := range sigs {
		var err error
		img, err = mutate.Append(img, mutate.Addendum{Layer: static.NewLayer(s.payload, SimpleSigningMediaType), Annotations: s.annotations})
		if err != nil {
			t.Fatal(err)
		}
	}
	tag := r.repo.Tag(strings.Replace(digest.DigestStr(), ":", "-", 1) + SignatureTagSuffix)
	if err := remote.Write(tag, img, r.opts()...); err != nil {
		t.Fatal(err)
	}
}

// testTSA is an in-test timestamp authority: a root and a leaf with the time-stamping usage.
type testTSA struct {
	chainPEM string
	leaf     *x509.Certificate
	leafKey  *ecdsa.PrivateKey
}

func newTestTSA(t *testing.T, validAt time.Time) *testTSA {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notBefore, notAfter := validAt.Add(-24*time.Hour), validAt.Add(24*time.Hour)
	rootTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test tsa root"},
		NotBefore: notBefore, NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTpl, rootTpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test tsa"},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(leafDER)
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	return &testTSA{chainPEM: chain, leaf: leaf, leafKey: leafKey}
}

// stamp returns the rfc3161timestamp annotation: a TimeStampResp over SHA-256 of sig at `at`.
func (a *testTSA) stamp(t *testing.T, at time.Time) func(sig []byte) string {
	return func(sig []byte) string {
		t.Helper()
		sum := sha256.Sum256(sig)
		ts := timestamp.Timestamp{
			HashAlgorithm: crypto.SHA256, HashedMessage: sum[:], Time: at,
			Policy: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 2}, AddTSACertificate: true,
		}
		der, err := ts.CreateResponseWithOpts(a.leaf, a.leafKey, crypto.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(struct {
			SignedRFC3161Timestamp []byte
		}{der})
		return string(raw)
	}
}

// keysJSON renders a signing-keys document from test keys.
func keysJSON(entries ...string) []byte {
	return []byte("[" + strings.Join(entries, ",") + "]")
}

func keyEntryJSON(kid, pubPEM string, tsaChain string, revokedAt *time.Time) string {
	e := map[string]any{"kid": kid, "publicKeyPem": pubPEM, "algorithm": "ecdsa-p256-sha256", "validFrom": "2020-01-01T00:00:00Z", "tsaChains": []any{}}
	if tsaChain != "" {
		e["tsaChains"] = []any{map[string]string{"name": "test", "pem": tsaChain}}
	}
	if revokedAt != nil {
		e["revokedAt"] = revokedAt.UTC().Format(time.RFC3339)
	}
	raw, _ := json.Marshal(e)
	return string(raw)
}
