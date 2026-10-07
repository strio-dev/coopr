package oci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/opencontainers/go-digest"
	"go.podman.io/storage"
)

type storedSignatureMetadata struct {
	SignatureSizes  []int                   `json:"signature-sizes,omitempty"`
	SignaturesSizes map[digest.Digest][]int `json:"signatures-sizes,omitempty"`
}

type storedSignatureState struct {
	metadata    string
	metadataRaw map[string]json.RawMessage
	parsed      storedSignatureMetadata
	defaultBlob []byte
	blobs       map[digest.Digest][]byte
}

// CaptureStoredSignatures snapshots native signature blobs and metadata.
func CaptureStoredSignatures(store storage.Store, imageID string) (storedSignatureState, error) {
	metadata, err := store.Metadata(imageID)
	if err != nil {
		return storedSignatureState{}, fmt.Errorf("read stored-image signature metadata: %w", err)
	}
	state := storedSignatureState{metadata: metadata, metadataRaw: map[string]json.RawMessage{}, blobs: map[digest.Digest][]byte{}}
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &state.parsed); err != nil {
			return storedSignatureState{}, fmt.Errorf("decode stored-image signature metadata: %w", err)
		}
		if err := json.Unmarshal([]byte(metadata), &state.metadataRaw); err != nil {
			return storedSignatureState{}, fmt.Errorf("preserve stored-image signature metadata: %w", err)
		}
	}
	if len(state.parsed.SignatureSizes) > 0 {
		state.defaultBlob, err = store.ImageBigData(imageID, "signatures")
		if err != nil {
			return storedSignatureState{}, fmt.Errorf("read default stored-image signatures: %w", err)
		}
	}
	for manifestDigest, sizes := range state.parsed.SignaturesSizes {
		if len(sizes) == 0 {
			continue
		}
		key, err := storedSignatureBigDataKey(manifestDigest)
		if err != nil {
			return storedSignatureState{}, err
		}
		blob, err := store.ImageBigData(imageID, key)
		if err != nil {
			return storedSignatureState{}, fmt.Errorf("read stored-image signatures for %s: %w", manifestDigest, err)
		}
		state.blobs[manifestDigest] = blob
	}
	return state, nil
}

// StoredSignatureBlobs reads one manifest signature set without conflating defaults.
func StoredSignatureBlobs(state storedSignatureState, selected digest.Digest, selectedIsDefault bool) ([][]byte, error) {
	sizes, found := state.parsed.SignaturesSizes[selected]
	blob := state.blobs[selected]
	instance := selected.String()
	if !found && selectedIsDefault {
		sizes = state.parsed.SignatureSizes
		blob = state.defaultBlob
		instance = "default instance"
	}
	result := make([][]byte, 0, len(sizes))
	offset := 0
	for _, size := range sizes {
		if size < 0 || size > len(blob)-offset {
			return nil, fmt.Errorf("stored-image signatures for %s have invalid size vector", instance)
		}
		result = append(result, bytes.Clone(blob[offset:offset+size]))
		offset += size
	}
	if offset != len(blob) {
		return nil, fmt.Errorf("stored-image signatures for %s contain %d unaccounted bytes", instance, len(blob)-offset)
	}
	return result, nil
}

// WriteStoredManifestSignatures updates one manifest while preserving other metadata.
func WriteStoredManifestSignatures(store storage.Store, imageID string, selected digest.Digest, selectedIsDefault bool, signatures [][]byte, previous storedSignatureState) error {
	metadata := previous.parsed
	if metadata.SignaturesSizes == nil {
		metadata.SignaturesSizes = map[digest.Digest][]int{}
	}
	sizes := make([]int, 0, len(signatures))
	combined := make([]byte, 0)
	for _, value := range signatures {
		sizes = append(sizes, len(value))
		combined = append(combined, value...)
	}
	metadata.SignaturesSizes[selected] = append([]int(nil), sizes...)
	key, err := storedSignatureBigDataKey(selected)
	if err != nil {
		return err
	}
	if err := store.SetImageBigData(imageID, key, combined, nil); err != nil {
		return fmt.Errorf("write stored-image signatures for %s: %w", selected, err)
	}
	if selectedIsDefault {
		metadata.SignatureSizes = append([]int(nil), sizes...)
		if err := store.SetImageBigData(imageID, "signatures", combined, nil); err != nil {
			return fmt.Errorf("write default stored-image signatures: %w", err)
		}
	}
	metadataRaw := make(map[string]json.RawMessage, len(previous.metadataRaw)+2)
	for key, value := range previous.metadataRaw {
		metadataRaw[key] = bytes.Clone(value)
	}
	perDigestSizes, err := json.Marshal(metadata.SignaturesSizes)
	if err != nil {
		return fmt.Errorf("encode per-manifest stored-image signature metadata: %w", err)
	}
	metadataRaw["signatures-sizes"] = perDigestSizes
	if selectedIsDefault {
		defaultSizes, err := json.Marshal(metadata.SignatureSizes)
		if err != nil {
			return fmt.Errorf("encode default stored-image signature metadata: %w", err)
		}
		metadataRaw["signature-sizes"] = defaultSizes
	}
	encoded, err := json.Marshal(metadataRaw)
	if err != nil {
		return fmt.Errorf("encode stored-image signature metadata: %w", err)
	}
	if err := store.SetMetadata(imageID, string(encoded)); err != nil {
		return fmt.Errorf("write stored-image signature metadata: %w", err)
	}
	return nil
}

// RestoreStoredSignatures restores a captured state after a failed mutation.
func RestoreStoredSignatures(store storage.Store, imageID string, previous storedSignatureState) error {
	var restoreErr error
	if len(previous.parsed.SignatureSizes) > 0 {
		restoreErr = errors.Join(restoreErr, store.SetImageBigData(imageID, "signatures", previous.defaultBlob, nil))
	}
	for manifestDigest, sizes := range previous.parsed.SignaturesSizes {
		if len(sizes) == 0 {
			continue
		}
		key, err := storedSignatureBigDataKey(manifestDigest)
		if err != nil {
			restoreErr = errors.Join(restoreErr, err)
			continue
		}
		restoreErr = errors.Join(restoreErr, store.SetImageBigData(imageID, key, previous.blobs[manifestDigest], nil))
	}
	restoreErr = errors.Join(restoreErr, store.SetMetadata(imageID, previous.metadata))
	return restoreErr
}

func storedSignatureBigDataKey(manifestDigest digest.Digest) (string, error) {
	if err := manifestDigest.Validate(); err != nil {
		return "", err
	}
	return "signature-" + manifestDigest.Encoded(), nil
}
