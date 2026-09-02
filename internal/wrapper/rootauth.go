package wrapper

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
)

const (
	minimumExportRootKeyLength = 32
	maximumExportRootKeyLength = 4096
)

type exportRootAuthorizer struct {
	configured  bool
	keyHash     [sha256.Size]byte
	fingerprint string
}

func loadExportRootAuthorizer(keyFile string) (exportRootAuthorizer, error) {
	if keyFile == "" {
		return exportRootAuthorizer{}, nil
	}
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return exportRootAuthorizer{}, fmt.Errorf("read export root key file: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if len(key) < minimumExportRootKeyLength {
		return exportRootAuthorizer{}, fmt.Errorf("export root key must contain at least %d characters", minimumExportRootKeyLength)
	}
	if len(key) > maximumExportRootKeyLength {
		return exportRootAuthorizer{}, fmt.Errorf("export root key must contain at most %d characters", maximumExportRootKeyLength)
	}
	for _, char := range []byte(key) {
		if char < 0x21 || char > 0x7e {
			return exportRootAuthorizer{}, fmt.Errorf("export root key must contain only visible ASCII characters")
		}
	}
	keyHash := sha256.Sum256([]byte(key))
	return exportRootAuthorizer{
		configured:  true,
		keyHash:     keyHash,
		fingerprint: hex.EncodeToString(keyHash[:]),
	}, nil
}

func (a exportRootAuthorizer) authorize(plan node.MountPlan, providedKey string) (string, error) {
	if !node.IsNFSExportRoot(plan) || !a.configured {
		return "", nil
	}
	providedHash := sha256.Sum256([]byte(strings.TrimSpace(providedKey)))
	if subtle.ConstantTimeCompare(a.keyHash[:], providedHash[:]) != 1 {
		return "", fmt.Errorf("mounting the NFS export root requires a valid export root key")
	}
	return a.fingerprint, nil
}

func (a exportRootAuthorizer) authorizesFingerprint(fingerprint string) bool {
	return a.configured && fingerprint != "" && fingerprint == a.fingerprint
}
