package credentials

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Rocket-Rescue-Node/credentials/pb"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"
)

type OperatorType = pb.OperatorType
type AuthenticatedCredential pb.AuthenticatedCredential

type jsonAuthenticatedCredential struct {
	NodeID       string       `json:"node_id"`
	Timestamp    int64        `json:"timestamp"`
	OperatorType OperatorType `json:"operator_type"`
	Mac          string       `json:"mac"`
}

func (ac *AuthenticatedCredential) Pb() *pb.AuthenticatedCredential {
	return (*pb.AuthenticatedCredential)(ac)
}

func (ac *AuthenticatedCredential) MarshalJSON() ([]byte, error) {
	var mac bytes.Buffer
	nodeID := "0x" + hex.EncodeToString(ac.Credential.NodeId)
	encoder := base64.NewEncoder(base64.URLEncoding, &mac)
	_, err := encoder.Write(ac.Mac)
	if err != nil {
		return nil, err
	}
	encoder.Close()

	return json.Marshal(&jsonAuthenticatedCredential{
		NodeID:       nodeID,
		Timestamp:    ac.Credential.Timestamp,
		OperatorType: ac.Credential.OperatorType,
		Mac:          mac.String(),
	})
}

func (ac *AuthenticatedCredential) UnmarshalJSON(data []byte) error {
	var j jsonAuthenticatedCredential
	ac.Pb().Reset()

	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	ac.Credential = &pb.Credential{}
	decoder := base64.NewDecoder(base64.URLEncoding, bytes.NewReader([]byte(j.Mac)))
	decoded, err := io.ReadAll(decoder)
	if err != nil {
		return err
	}

	nodeID, err := hex.DecodeString(strings.TrimPrefix(j.NodeID, "0x"))
	if err != nil {
		return err
	}

	ac.Credential.NodeId = nodeID
	ac.Credential.OperatorType = j.OperatorType
	ac.Credential.Timestamp = j.Timestamp
	ac.Mac = decoded
	return nil
}

func (ac *AuthenticatedCredential) Base64URLEncodeUsername() string {
	var out bytes.Buffer

	encoder := base64.NewEncoder(base64.URLEncoding, &out)
	_, err := encoder.Write(ac.Credential.NodeId)
	if err != nil {
		return ""
	}
	encoder.Close()

	return out.String()
}

func (ac *AuthenticatedCredential) Base64URLEncodePassword() (string, error) {
	var out bytes.Buffer

	// Save the nodeId
	nodeID := ac.Credential.NodeId
	// Strip it to save space
	ac.Credential.NodeId = nil

	marshaled, err := proto.Marshal(ac.Pb())
	if err != nil {
		return "", err
	}

	// Restore the nodeId
	ac.Credential.NodeId = nodeID

	// Encode the marshaled proto
	encoder := base64.NewEncoder(base64.URLEncoding, &out)
	_, err = encoder.Write(marshaled)
	if err != nil {
		return "", err
	}
	encoder.Close()

	return out.String(), nil
}

func (ac *AuthenticatedCredential) Base64URLDecode(username string, password string) error {
	decoder := base64.NewDecoder(base64.URLEncoding, bytes.NewReader([]byte(username)))
	nodeID, err := io.ReadAll(decoder)
	if err != nil {
		return err
	}

	decoder = base64.NewDecoder(base64.URLEncoding, bytes.NewReader([]byte(password)))
	decoded, err := io.ReadAll(decoder)
	if err != nil {
		return err
	}

	newCred := AuthenticatedCredential{}
	err = proto.Unmarshal(decoded, newCred.Pb())
	if err != nil {
		return err
	}

	ac.Pb().Reset()
	proto.Merge(ac.Pb(), newCred.Pb())
	ac.Credential.NodeId = nodeID
	return nil
}

// CredentialManager authenticates and verifies rescue node credentials
type CredentialManager struct {
	sync.Pool
}

// NewCredentialManager creates a new CredentialManager which can create and verify authenticated credentials
func NewCredentialManager(key []byte) *CredentialManager {
	return &CredentialManager{
		sync.Pool{
			New: func() any {
				return hmac.New(sha256.New, key)
			},
		},
	}
}

func (c *CredentialManager) authenticateCredential(credential *AuthenticatedCredential) error {
	// Serialize just the inner message so we can authenticate it and add it to the outer message
	bytes, err := proto.Marshal(credential.Credential)
	if err != nil {
		return errors.Wrap(err, "Error serializing HMAC protobuf body")
	}

	h, ok := c.Get().(hash.Hash)
	if !ok {
		return errors.New("Couldn't retrieve available hash from pool")
	}

	h.Write(bytes)
	credential.Mac = h.Sum(nil)
	h.Reset()
	c.Put(h)

	return nil
}

// Create makes a new credential and authenticates it, returning a protoc struct that can be marshaled/unmarshaled
func (c *CredentialManager) Create(timestamp time.Time, nodeID []byte, OperatorType OperatorType) (*AuthenticatedCredential, error) {
	if len(nodeID) != 20 {
		return nil, fmt.Errorf("invalid nodeID length. Expected 20, got %d", len(nodeID))
	}
	message := AuthenticatedCredential{}
	message.Credential = &pb.Credential{}
	message.Credential.NodeId = nodeID
	message.Credential.OperatorType = OperatorType
	message.Credential.Timestamp = timestamp.Unix()

	if err := c.authenticateCredential(&message); err != nil {
		return nil, err
	}

	return &message, nil
}

// Verify checks that a AuthenticatedCredential has a valid mac
func (c *CredentialManager) Verify(authenticatedCredential *AuthenticatedCredential) error {
	// Create a temporary AuthenticatedCredential and borrow the inner message from the provided credential
	tmp := AuthenticatedCredential{}
	tmp.Credential = authenticatedCredential.Credential

	// Auth tmp
	if err := c.authenticateCredential(&tmp); err != nil {
		return errors.Wrap(err, "Error while re-creating the MAC")
	}

	// Check that tmp's MAC matches the provided one.
	if !hmac.Equal(tmp.Mac, authenticatedCredential.Mac) {
		// MAC didn't match. Authenticity cannot be verified.
		return errors.New("credential MAC mismatch")
	}

	return nil
}
