package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"

	beamauth "dylaris-pkg/beam/auth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// coreDialCreds returns the transport-credentials dial option for every
// node->core gRPC dial. With GRPC_TLS_ENABLED off it is plaintext (today's
// behavior). With it on it is server-authenticated TLS with the Core cert
// fingerprint pinned: the cert is derived cluster-wide from CLUSTER_SECRET, so
// the node derives the same expected fingerprint locally and rejects anything
// else. There is no silent downgrade - a pin failure fails the dial.
func coreDialCreds() grpc.DialOption {
	if !grpcTLSEnabled {
		return grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	_, pinnedFP, err := resolveCorePin()
	if err != nil {
		// main() resolves the same pin at boot and exits there, so reaching this
		// means the configuration changed under a running process. Refuse the
		// dial rather than silently downgrading to plaintext.
		log.Printf("core gRPC: refusing to dial - %v", err)
		return grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true,
			VerifyConnection: func(tls.ConnectionState) error {
				return errors.New("core gRPC: no certificate pin configured")
			},
		}))
	}
	cfg := &tls.Config{
		InsecureSkipVerify: true, // we pin instead of CA/hostname chain-verify
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("core gRPC: no peer certificate")
			}
			got := hex.EncodeToString(sha256Sum(cs.PeerCertificates[0].Raw))
			if got != pinnedFP {
				// Both values, not just "mismatch". The operator's next question
				// is always which end is wrong, and that is answerable only by
				// comparing this against the fp= prefix Core logs at startup.
				return fmt.Errorf("core gRPC: certificate fingerprint mismatch (Core served %s..., this node expects %s...); "+
					"the two ends hold different CLUSTER_SECRETs, or GRPC_TLS_FINGERPRINT is stale - re-copy the deploy snippet",
					got[:16], pinnedFP[:16])
			}
			return nil
		},
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(cfg))
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// resolveCorePin returns where the expected Core certificate pin comes from and
// its lowercase-hex value.
//
// A node in the cluster holds CLUSTER_SECRET and derives the same certificate
// Core does. A BYON node holds no fleet secret and pins the fingerprint handed
// to it out of band (public pinning material, not a secret), which the panel
// writes into the deploy snippet.
//
// Split out of coreDialCreds so main() can fail at BOOT on a missing pin instead
// of at the first dial, and so the boot log can name the source: "which of the
// two sources am I using" is the first thing to establish when a pin does not
// match.
func resolveCorePin() (source, fingerprint string, err error) {
	switch {
	case clusterSecret != "":
		fp, derr := beamauth.ClusterGRPCCertFingerprint(clusterSecret)
		if derr != nil {
			return "", "", fmt.Errorf("cannot derive the pin from CLUSTER_SECRET: %w", derr)
		}
		return "CLUSTER_SECRET", fp, nil
	case grpcTLSFingerprint != "":
		// Normalised to what VerifyConnection compares against (lowercase hex,
		// no colons) and checked here, so a mangled paste fails boot with the
		// FATAL that names the fix instead of never matching or panicking.
		fp := strings.ToLower(strings.ReplaceAll(grpcTLSFingerprint, ":", ""))
		if b, herr := hex.DecodeString(fp); herr != nil || len(b) != sha256.Size {
			return "", "", fmt.Errorf("GRPC_TLS_FINGERPRINT is not a SHA-256 fingerprint (want %d hex characters)", 2*sha256.Size)
		}
		return "GRPC_TLS_FINGERPRINT", fp, nil
	default:
		return "", "", errors.New("neither CLUSTER_SECRET nor GRPC_TLS_FINGERPRINT is set")
	}
}
