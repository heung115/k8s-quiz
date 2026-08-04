package problem

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

type artifactCodecImageResolver struct {
	contentID string
}

func (r artifactCodecImageResolver) ResolveImage(context.Context, string) (string, error) {
	return r.contentID, nil
}

func TestRuntimeArtifactCodecDeterministicGoldenRoundTrip(t *testing.T) {
	artifact, wantBundle := runtimeArtifactCodecFixture(t, true)

	first, firstRef, err := EncodeRuntimeArtifact(artifact)
	if err != nil {
		t.Fatalf("encode first artifact: %v", err)
	}
	second, secondRef, err := EncodeRuntimeArtifact(artifact)
	if err != nil {
		t.Fatalf("encode second artifact: %v", err)
	}
	if !bytes.Equal(first, second) || firstRef != secondRef {
		t.Fatal("same runtime artifact did not produce deterministic bytes and reference")
	}
	if firstRef.DigestSchema != ArtifactDigestSchemaV1 || firstRef.MediaType != RuntimeArtifactMediaTypeV1 || firstRef.Size != int64(len(first)) {
		t.Fatalf("unexpected artifact reference: %+v", firstRef)
	}
	const goldenDigest = "295247b297808cb1644accaa12eabacf3047eab610df3e9b6da953861e1c9541"
	if got := hex.EncodeToString(firstRef.Digest[:]); got != goldenDigest {
		t.Fatalf("artifact digest = %s, want golden %s", got, goldenDigest)
	}

	decoded, gotBundle, err := DecodeRuntimeArtifact(firstRef, first)
	if err != nil {
		t.Fatalf("decode canonical artifact: %v", err)
	}
	if !reflect.DeepEqual(decoded, artifact) {
		t.Fatalf("decoded capture differs\ngot:  %#v\nwant: %#v", decoded, artifact)
	}
	if !reflect.DeepEqual(gotBundle, wantBundle) {
		t.Fatalf("decoded bundle differs\ngot:  %#v\nwant: %#v", gotBundle, wantBundle)
	}

	// Returned values and encoded bytes must not retain caller-owned slices.
	decoded.Manifest[0] ^= 0xff
	decoded.Setup.Data[0] ^= 0xff
	decoded.ImagesLock[0] ^= 0xff
	decoded.Problem.Choices[0].Text = "mutated"
	again, againBundle, err := DecodeRuntimeArtifact(firstRef, first)
	if err != nil {
		t.Fatalf("decode after mutating returned capture: %v", err)
	}
	if !reflect.DeepEqual(again, artifact) || !reflect.DeepEqual(againBundle, wantBundle) {
		t.Fatal("decoded capture or bundle aliases a prior return value")
	}
}

func TestRuntimeArtifactCodecPreservesMissingAndPresentEmpty(t *testing.T) {
	present, _ := runtimeArtifactCodecFixture(t, true)
	missing, _ := runtimeArtifactCodecFixture(t, false)

	presentBytes, presentRef, err := EncodeRuntimeArtifact(present)
	if err != nil {
		t.Fatal(err)
	}
	missingBytes, missingRef, err := EncodeRuntimeArtifact(missing)
	if err != nil {
		t.Fatal(err)
	}
	if present.Revision == missing.Revision {
		t.Fatal("legacy revision normalised present-empty and missing verify.sh")
	}
	if presentRef == missingRef || bytes.Equal(presentBytes, missingBytes) {
		t.Fatal("canonical artifact normalised present-empty and missing verify.sh")
	}

	decodedPresent, _, err := DecodeRuntimeArtifact(presentRef, presentBytes)
	if err != nil {
		t.Fatal(err)
	}
	decodedMissing, _, err := DecodeRuntimeArtifact(missingRef, missingBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !decodedPresent.Verify.Present || decodedPresent.Verify.Data == nil || len(decodedPresent.Verify.Data) != 0 {
		t.Fatalf("present-empty verify.sh was not retained: %#v", decodedPresent.Verify)
	}
	if decodedMissing.Verify.Present || decodedMissing.Verify.Data != nil {
		t.Fatalf("missing verify.sh was not retained: %#v", decodedMissing.Verify)
	}
}

func TestRuntimeArtifactCodecRejectsReferenceAndByteTampering(t *testing.T) {
	artifact, _ := runtimeArtifactCodecFixture(t, true)
	data, ref, err := EncodeRuntimeArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		ref  ArtifactRef
		data []byte
	}{
		{name: "digest schema", ref: mutateArtifactRef(ref, func(ref *ArtifactRef) { ref.DigestSchema++ }), data: data},
		{name: "media type", ref: mutateArtifactRef(ref, func(ref *ArtifactRef) { ref.MediaType += "+unknown" }), data: data},
		{name: "size", ref: mutateArtifactRef(ref, func(ref *ArtifactRef) { ref.Size++ }), data: data},
		{name: "digest", ref: mutateArtifactRef(ref, func(ref *ArtifactRef) { ref.Digest[0] ^= 0xff }), data: data},
		{name: "content", ref: ref, data: mutateArtifactBytes(data, func(data []byte) { data[len(data)/2] ^= 0xff })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := DecodeRuntimeArtifact(tc.ref, tc.data); !errors.Is(err, ErrInvalidRuntimeArtifact) {
				t.Fatalf("error = %v, want ErrInvalidRuntimeArtifact", err)
			}
		})
	}
}

func TestRuntimeArtifactCodecRejectsAuthenticatedStructuralTampering(t *testing.T) {
	artifact, _ := runtimeArtifactCodecFixture(t, true)
	canonical, _, err := EncodeRuntimeArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "unknown format schema",
			mutate: func(data []byte) []byte {
				binary.BigEndian.PutUint16(data[len(runtimeArtifactMagic):], runtimeArtifactFormatV1+1)
				return data
			},
		},
		{
			name: "unexpected record count",
			mutate: func(data []byte) []byte {
				binary.BigEndian.PutUint16(data[len(runtimeArtifactMagic)+2:], runtimeArtifactRecordCountV1+1)
				return data
			},
		},
		{
			name: "wrong fixed record tag",
			mutate: func(data []byte) []byte {
				binary.BigEndian.PutUint16(data[len(runtimeArtifactMagic)+4:], artifactRecordManifest)
				return data
			},
		},
		{
			name:   "trailing data",
			mutate: func(data []byte) []byte { return append(data, 0) },
		},
		{
			name: "projection differs from manifest",
			mutate: func(data []byte) []byte {
				old := []byte("Codec Fixture")
				position := bytes.LastIndex(data, old)
				if position < 0 {
					t.Fatal("fixture title not found in projection")
				}
				copy(data[position:], []byte("Codec Forgery"))
				return data
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mutate(append([]byte(nil), canonical...))
			ref := runtimeArtifactRef(mutated)
			if _, _, err := DecodeRuntimeArtifact(ref, mutated); !errors.Is(err, ErrInvalidRuntimeArtifact) {
				t.Fatalf("error = %v, want ErrInvalidRuntimeArtifact", err)
			}
		})
	}
}

func TestRuntimeArtifactCodecRejectsOversizedLengthsBeforeAllocation(t *testing.T) {
	var data bytes.Buffer
	data.WriteString(runtimeArtifactMagic)
	writeUint16(&data, runtimeArtifactFormatV1)
	writeUint16(&data, runtimeArtifactRecordCountV1)
	writeUint16(&data, artifactRecordProblemID)
	writeUint64(&data, math.MaxUint64)

	encoded := data.Bytes()
	ref := runtimeArtifactRef(encoded)
	if _, _, err := DecodeRuntimeArtifact(ref, encoded); !errors.Is(err, ErrInvalidRuntimeArtifact) || !strings.Contains(err.Error(), "length") {
		t.Fatalf("oversized record error = %v, want bounded length rejection", err)
	}
}

func TestRuntimeArtifactProjectionRejectsImpossibleChoiceCountBeforeAllocation(t *testing.T) {
	var projection bytes.Buffer
	writeUint16(&projection, problemProjectionSchemaV1)
	writeUint16(&projection, problemProjectionFieldCountV1)
	for range 7 {
		writeProjectionString(&projection, "")
	}
	writeUint64(&projection, 0)
	for range 3 {
		writeProjectionString(&projection, "")
	}
	writeUint32(&projection, maxArtifactChoiceCount)

	if _, err := decodeProblemProjection(projection.Bytes()); !errors.Is(err, ErrInvalidRuntimeArtifact) || !strings.Contains(err.Error(), "too many choices") {
		t.Fatalf("impossible choice count error = %v, want pre-allocation rejection", err)
	}
}

func TestRuntimeArtifactCodecRejectsInvalidCapturedSemantics(t *testing.T) {
	base, _ := runtimeArtifactCodecFixture(t, true)
	tests := []struct {
		name   string
		mutate func(*CapturedRuntimeArtifact)
	}{
		{name: "unsupported trust", mutate: func(a *CapturedRuntimeArtifact) { a.SourceTrust = "signed-but-unknown" }},
		{name: "mutable runtime image", mutate: func(a *CapturedRuntimeArtifact) { a.RuntimeImage = "k3s-base:latest" }},
		{name: "catalog state", mutate: func(a *CapturedRuntimeArtifact) { a.Problem.CatalogActive = true }},
		{name: "revision", mutate: func(a *CapturedRuntimeArtifact) { a.Revision = strings.Repeat("0", 64) }},
		{name: "projection", mutate: func(a *CapturedRuntimeArtifact) { a.Problem.Title = "forged title" }},
		{name: "missing optional with bytes", mutate: func(a *CapturedRuntimeArtifact) {
			a.Verify = ArtifactOptionalFile{Data: []byte("hidden")}
		}},
		{name: "unapproved workload image", mutate: func(a *CapturedRuntimeArtifact) {
			a.Setup = ArtifactOptionalFile{Present: true, Data: []byte("kubectl create deployment x --image=nginx:latest\n")}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := cloneCapturedRuntimeArtifact(base)
			tc.mutate(&candidate)
			if _, _, err := EncodeRuntimeArtifact(candidate); !errors.Is(err, ErrInvalidRuntimeArtifact) {
				t.Fatalf("error = %v, want ErrInvalidRuntimeArtifact", err)
			}
		})
	}
}

func runtimeArtifactCodecFixture(t *testing.T, verifyPresent bool) (CapturedRuntimeArtifact, RuntimeBundle) {
	t.Helper()
	lockedImage := "registry.example.invalid/workload:v1@sha256:" + strings.Repeat("b", 64)
	imagesLock := []byte("workload=" + lockedImage + "\n")
	manifest := []byte(`id: codec-problem
title: "Codec Fixture"
description: "Canonical runtime artifact"
category: config
difficulty: medium
type: find
timeout_minutes: 19
verify_type: choice
base_image: k3s-base:latest
choices:
  - id: inspect
    text: "Inspect the workload"
  - id: restart
    text: "Restart the workload"
correct_choice: inspect
grading_prompt: "Explain the invariant"
`)
	files := map[string][]byte{
		"problem.yaml": manifest,
		"setup.sh":     []byte("#!/bin/sh\nkubectl create deployment fixture --image=" + lockedImage + "\n"),
		"hint.md":      []byte("Inspect the selected image.\n"),
	}
	if verifyPresent {
		files["verify.sh"] = []byte{}
	} else {
		files["verify.sh"] = nil
	}
	policy, err := parseApprovedImagePolicy(imagesLock)
	if err != nil {
		t.Fatal(err)
	}
	loader := &GitLoader{
		strictRuntime: true,
		imageResolver: artifactCodecImageResolver{contentID: "sha256:" + strings.Repeat("a", 64)},
	}
	bundle, err := loader.buildRuntimeBundle(context.Background(), "codec-problem", files, imagesLock, policy)
	if err != nil {
		t.Fatalf("build fixture runtime bundle: %v", err)
	}
	if msg := validateLoadedProblem(&bundle.Problem); msg != "" {
		t.Fatalf("fixture problem is invalid: %s", msg)
	}
	artifact, err := NewCapturedRuntimeArtifact(files, imagesLock, bundle)
	if err != nil {
		t.Fatalf("capture fixture runtime artifact: %v", err)
	}
	return artifact, bundle
}

func mutateArtifactRef(ref ArtifactRef, mutate func(*ArtifactRef)) ArtifactRef {
	mutate(&ref)
	return ref
}

func mutateArtifactBytes(data []byte, mutate func([]byte)) []byte {
	copy := append([]byte(nil), data...)
	mutate(copy)
	return copy
}
