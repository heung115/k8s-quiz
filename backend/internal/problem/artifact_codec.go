package problem

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"

	"github.com/k8s-quiz/backend/pkg/models"
	"gopkg.in/yaml.v3"
)

// ArtifactRef is the immutable, provider-neutral identity of one canonical
// runtime artifact. DigestSchema describes how Digest is calculated; the
// artifact's own format schema is encoded and independently checked in Data.
type ArtifactRef struct {
	DigestSchema uint16
	Digest       [sha256.Size]byte
	MediaType    string
	Size         int64
}

const (
	ArtifactDigestSchemaV1     uint16 = 1
	RuntimeArtifactMediaTypeV1        = "application/vnd.k8s-quiz.problem-runtime.v1"
)

const (
	runtimeArtifactMagic         = "k8s-quiz.problem-runtime-artifact\x00"
	runtimeArtifactFormatV1      = uint16(1)
	runtimeArtifactRecordCountV1 = uint16(10)
	runtimeArtifactDigestDomain  = runtimeArtifactMagic
	maxProblemProjectionBytes    = maxProblemManifestBytes + maxProblemHintBytes + 64<<10
	maxRuntimeImageBytes         = 71 // sha256:<64 lowercase hexadecimal characters>
	maxArtifactIdentityBytes     = 64
	maxArtifactTrustBytes        = 64
	maxArtifactChoiceCount       = maxProblemManifestBytes / 4
)

const (
	artifactRecordProblemID uint16 = iota + 1
	artifactRecordManifest
	artifactRecordSetup
	artifactRecordVerify
	artifactRecordHint
	artifactRecordImagesLock
	artifactRecordRuntimeImage
	artifactRecordSourceTrust
	artifactRecordRevision
	artifactRecordProblemProjection
)

const (
	problemProjectionSchemaV1     uint16 = 1
	problemProjectionFieldCountV1 uint16 = 15
)

var ErrInvalidRuntimeArtifact = errors.New("invalid problem runtime artifact")

// ArtifactOptionalFile retains the distinction between a missing optional
// file and an explicitly present zero-byte file. That distinction participates
// in the legacy problem revision and therefore cannot be normalised away.
type ArtifactOptionalFile struct {
	Present bool
	Data    []byte
}

// CapturedRuntimeArtifact contains every source byte and derived value needed
// to reconstruct an exact RuntimeBundle after a process restart. Callers must
// use NewCapturedRuntimeArtifact for checkout captures; decoded values have
// already passed the same validation in DecodeRuntimeArtifact.
type CapturedRuntimeArtifact struct {
	Manifest     []byte
	Setup        ArtifactOptionalFile
	Verify       ArtifactOptionalFile
	Hint         ArtifactOptionalFile
	ImagesLock   []byte
	Problem      models.Problem
	Revision     string
	RuntimeImage string
	SourceTrust  BundleSourceTrust
}

// NewCapturedRuntimeArtifact binds the raw checkout bytes to the RuntimeBundle
// derived from them. Nil optional map values are treated as missing, matching
// the existing loader's frozen revision algorithm; a non-nil empty slice is an
// explicitly present empty file.
func NewCapturedRuntimeArtifact(files map[string][]byte, imagesLock []byte, bundle RuntimeBundle) (CapturedRuntimeArtifact, error) {
	manifest, ok := files["problem.yaml"]
	if !ok || manifest == nil {
		return CapturedRuntimeArtifact{}, invalidArtifactf("problem.yaml is required")
	}
	if imagesLock == nil {
		return CapturedRuntimeArtifact{}, invalidArtifactf("images.lock is required")
	}

	artifact := CapturedRuntimeArtifact{
		Manifest:     cloneArtifactBytes(manifest),
		Setup:        captureArtifactOptional(files, "setup.sh"),
		Verify:       captureArtifactOptional(files, "verify.sh"),
		Hint:         captureArtifactOptional(files, "hint.md"),
		ImagesLock:   cloneArtifactBytes(imagesLock),
		Problem:      cloneProblem(bundle.Problem),
		Revision:     bundle.Revision,
		RuntimeImage: bundle.RuntimeImage,
		SourceTrust:  bundle.SourceTrust,
	}
	rebuilt, err := validateCapturedRuntimeArtifact(artifact)
	if err != nil {
		return CapturedRuntimeArtifact{}, err
	}
	if rebuilt.SetupScript != bundle.SetupScript || rebuilt.VerifyScript != bundle.VerifyScript ||
		rebuilt.Revision != bundle.Revision || rebuilt.RuntimeImage != bundle.RuntimeImage ||
		rebuilt.SourceTrust != bundle.SourceTrust || !runtimeProblemProjectionEqual(rebuilt.Problem, bundle.Problem) {
		return CapturedRuntimeArtifact{}, invalidArtifactf("runtime bundle does not match captured source bytes")
	}
	return cloneCapturedRuntimeArtifact(artifact), nil
}

// EncodeRuntimeArtifact validates and deterministically encodes one artifact.
// The returned reference is over the canonical bytes and uses a digest domain
// distinct from legacy problem revisions and catalog publication identities.
func EncodeRuntimeArtifact(artifact CapturedRuntimeArtifact) ([]byte, ArtifactRef, error) {
	if _, err := validateCapturedRuntimeArtifact(artifact); err != nil {
		return nil, ArtifactRef{}, err
	}

	projection, err := encodeProblemProjection(artifact.Problem)
	if err != nil {
		return nil, ArtifactRef{}, err
	}
	var out bytes.Buffer
	out.Grow(len(runtimeArtifactMagic) + 256 + len(artifact.Manifest) + len(artifact.Setup.Data) +
		len(artifact.Verify.Data) + len(artifact.Hint.Data) + len(artifact.ImagesLock) + len(projection))
	out.WriteString(runtimeArtifactMagic)
	writeUint16(&out, runtimeArtifactFormatV1)
	writeUint16(&out, runtimeArtifactRecordCountV1)
	writeArtifactRecord(&out, artifactRecordProblemID, []byte(artifact.Problem.ID))
	writeArtifactRecord(&out, artifactRecordManifest, artifact.Manifest)
	writeArtifactRecord(&out, artifactRecordSetup, encodeArtifactOptional(artifact.Setup))
	writeArtifactRecord(&out, artifactRecordVerify, encodeArtifactOptional(artifact.Verify))
	writeArtifactRecord(&out, artifactRecordHint, encodeArtifactOptional(artifact.Hint))
	writeArtifactRecord(&out, artifactRecordImagesLock, artifact.ImagesLock)
	writeArtifactRecord(&out, artifactRecordRuntimeImage, []byte(artifact.RuntimeImage))
	writeArtifactRecord(&out, artifactRecordSourceTrust, []byte(artifact.SourceTrust))
	writeArtifactRecord(&out, artifactRecordRevision, []byte(artifact.Revision))
	writeArtifactRecord(&out, artifactRecordProblemProjection, projection)
	if out.Len() > maxRuntimeArtifactBytes {
		return nil, ArtifactRef{}, invalidArtifactf("canonical bytes exceed the %d byte limit", maxRuntimeArtifactBytes)
	}
	data := append([]byte(nil), out.Bytes()...)
	return data, runtimeArtifactRef(data), nil
}

// DecodeRuntimeArtifact authenticates the supplied bytes against ref, parses
// the fixed v1 record sequence with allocation bounds, rebuilds all derived
// runtime semantics from raw sources, and finally requires byte-for-byte
// canonical re-encoding.
func DecodeRuntimeArtifact(ref ArtifactRef, data []byte) (CapturedRuntimeArtifact, RuntimeBundle, error) {
	if err := validateRuntimeArtifactRef(ref, data); err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	if len(data) > maxRuntimeArtifactBytes {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactf("canonical bytes exceed the %d byte limit", maxRuntimeArtifactBytes)
	}

	r := bytes.NewReader(data)
	magic := make([]byte, len(runtimeArtifactMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactWrap("read magic", err)
	}
	if string(magic) != runtimeArtifactMagic {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactf("unknown magic")
	}
	format, err := readUint16(r)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactWrap("read format schema", err)
	}
	if format != runtimeArtifactFormatV1 {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactf("unknown format schema %d", format)
	}
	recordCount, err := readUint16(r)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactWrap("read record count", err)
	}
	if recordCount != runtimeArtifactRecordCountV1 {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactf("record count is %d, want %d", recordCount, runtimeArtifactRecordCountV1)
	}

	problemID, err := readArtifactRecord(r, artifactRecordProblemID, maxArtifactIdentityBytes)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	manifest, err := readArtifactRecord(r, artifactRecordManifest, maxProblemManifestBytes)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	setupRecord, err := readArtifactRecord(r, artifactRecordSetup, maxProblemScriptBytes+1)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	verifyRecord, err := readArtifactRecord(r, artifactRecordVerify, maxProblemScriptBytes+1)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	hintRecord, err := readArtifactRecord(r, artifactRecordHint, maxProblemHintBytes+1)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	imagesLock, err := readArtifactRecord(r, artifactRecordImagesLock, maxImageLockBytes)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	runtimeImage, err := readArtifactRecord(r, artifactRecordRuntimeImage, maxRuntimeImageBytes)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	sourceTrust, err := readArtifactRecord(r, artifactRecordSourceTrust, maxArtifactTrustBytes)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	revision, err := readArtifactRecord(r, artifactRecordRevision, sha256.Size*2)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	projectionBytes, err := readArtifactRecord(r, artifactRecordProblemProjection, maxProblemProjectionBytes)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	if r.Len() != 0 {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactf("trailing data after fixed record sequence")
	}

	setup, err := decodeArtifactOptional("setup.sh", setupRecord)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	verify, err := decodeArtifactOptional("verify.sh", verifyRecord)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	hint, err := decodeArtifactOptional("hint.md", hintRecord)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	projection, err := decodeProblemProjection(projectionBytes)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	if projection.ID != string(problemID) {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactf("problem identity record does not match projection")
	}

	artifact := CapturedRuntimeArtifact{
		Manifest:     manifest,
		Setup:        setup,
		Verify:       verify,
		Hint:         hint,
		ImagesLock:   imagesLock,
		Problem:      projection,
		Revision:     string(revision),
		RuntimeImage: string(runtimeImage),
		SourceTrust:  BundleSourceTrust(sourceTrust),
	}
	bundle, err := validateCapturedRuntimeArtifact(artifact)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	canonical, canonicalRef, err := EncodeRuntimeArtifact(artifact)
	if err != nil {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, err
	}
	if canonicalRef != ref || !bytes.Equal(canonical, data) {
		return CapturedRuntimeArtifact{}, RuntimeBundle{}, invalidArtifactf("encoding is not canonical")
	}
	return cloneCapturedRuntimeArtifact(artifact), cloneRuntimeBundle(bundle), nil
}

func validateCapturedRuntimeArtifact(artifact CapturedRuntimeArtifact) (RuntimeBundle, error) {
	if artifact.Manifest == nil {
		return RuntimeBundle{}, invalidArtifactf("problem.yaml is required")
	}
	if artifact.ImagesLock == nil {
		return RuntimeBundle{}, invalidArtifactf("images.lock is required")
	}
	if len(artifact.Manifest) > maxProblemManifestBytes {
		return RuntimeBundle{}, invalidArtifactf("problem.yaml exceeds the %d byte limit", maxProblemManifestBytes)
	}
	if len(artifact.ImagesLock) > maxImageLockBytes {
		return RuntimeBundle{}, invalidArtifactf("images.lock exceeds the %d byte limit", maxImageLockBytes)
	}
	if err := validateArtifactOptional("setup.sh", artifact.Setup, maxProblemScriptBytes); err != nil {
		return RuntimeBundle{}, err
	}
	if err := validateArtifactOptional("verify.sh", artifact.Verify, maxProblemScriptBytes); err != nil {
		return RuntimeBundle{}, err
	}
	if err := validateArtifactOptional("hint.md", artifact.Hint, maxProblemHintBytes); err != nil {
		return RuntimeBundle{}, err
	}
	rawTotal := len(artifact.Manifest) + len(artifact.Setup.Data) + len(artifact.Verify.Data) + len(artifact.Hint.Data) + len(artifact.ImagesLock)
	if rawTotal > maxRuntimeArtifactBytes {
		return RuntimeBundle{}, invalidArtifactf("runtime source bytes exceed the %d byte limit", maxRuntimeArtifactBytes)
	}
	if artifact.SourceTrust != BundleSourceDevelopmentCheckout {
		return RuntimeBundle{}, invalidArtifactf("source trust %q is not supported", artifact.SourceTrust)
	}
	if !validImageContentID(artifact.RuntimeImage) {
		return RuntimeBundle{}, invalidArtifactf("runtime image %q is not an immutable content ID", artifact.RuntimeImage)
	}
	if artifact.Problem.CatalogActive || !artifact.Problem.CreatedAt.IsZero() || !artifact.Problem.UpdatedAt.IsZero() {
		return RuntimeBundle{}, invalidArtifactf("catalog state and timestamps are prohibited in the runtime projection")
	}

	files := map[string][]byte{
		"problem.yaml": cloneArtifactBytes(artifact.Manifest),
		"setup.sh":     artifactOptionalBytes(artifact.Setup),
		"verify.sh":    artifactOptionalBytes(artifact.Verify),
		"hint.md":      artifactOptionalBytes(artifact.Hint),
	}
	var py models.ProblemYAML
	if err := yaml.Unmarshal(files["problem.yaml"], &py); err != nil {
		return RuntimeBundle{}, invalidArtifactWrap("parse problem.yaml", err)
	}
	problem := models.Problem{
		ID:             py.ID,
		Title:          py.Title,
		Description:    py.Description,
		Category:       py.Category,
		Difficulty:     py.Difficulty,
		Type:           py.Type,
		TimeoutMinutes: py.TimeoutMinutes,
		VerifyType:     py.VerifyType,
		BaseImage:      py.BaseImage,
		Image:          py.Image,
		Choices:        append([]models.Choice(nil), py.Choices...),
		CorrectChoice:  py.CorrectChoice,
		Hint:           string(files["hint.md"]),
		GradingPrompt:  py.GradingPrompt,
	}
	if problem.TimeoutMinutes == 0 {
		problem.TimeoutMinutes = 30
	}
	if problem.BaseImage == "" && problem.Image == "" {
		problem.BaseImage = models.DefaultBaseImage
	}
	policy, err := parseApprovedImagePolicy(artifact.ImagesLock)
	if err != nil {
		return RuntimeBundle{}, invalidArtifactWrap("parse images.lock", err)
	}
	if err := validateApprovedImagePolicy(files, policy); err != nil {
		return RuntimeBundle{}, invalidArtifactWrap("validate workload images", err)
	}
	if problem.VerifyType == "script" && strings.TrimSpace(string(files["verify.sh"])) == "" {
		return RuntimeBundle{}, invalidArtifactf("script problem requires a non-empty verify.sh")
	}
	if msg := validateLoadedProblem(&problem); msg != "" {
		return RuntimeBundle{}, invalidArtifactf("problem projection is invalid: %s", msg)
	}

	revision := frozenLegacyRuntimeRevision(artifact.Manifest, artifact.Setup, artifact.Verify, artifact.Hint, artifact.ImagesLock, artifact.RuntimeImage)
	problem.Revision = revision
	if artifact.Revision != revision || artifact.Problem.Revision != revision {
		return RuntimeBundle{}, invalidArtifactf("legacy revision does not match captured source bytes")
	}
	if !runtimeProblemProjectionEqual(problem, artifact.Problem) {
		return RuntimeBundle{}, invalidArtifactf("problem projection does not match captured source bytes")
	}
	return RuntimeBundle{
		Problem:      problem,
		SetupScript:  string(files["setup.sh"]),
		VerifyScript: string(files["verify.sh"]),
		Revision:     revision,
		RuntimeImage: artifact.RuntimeImage,
		SourceTrust:  artifact.SourceTrust,
	}, nil
}

func frozenLegacyRuntimeRevision(manifest []byte, setup, verify, hint ArtifactOptionalFile, imagesLock []byte, runtimeImage string) string {
	h := sha256.New()
	writeLegacyRevisionFile(h, "problem.yaml", true, manifest)
	writeLegacyRevisionFile(h, "setup.sh", setup.Present, setup.Data)
	writeLegacyRevisionFile(h, "verify.sh", verify.Present, verify.Data)
	writeLegacyRevisionFile(h, "hint.md", hint.Present, hint.Data)
	// Runtime artifacts require both values, so they always use the legacy
	// present branches below. The byte stream intentionally mirrors loader.go.
	fmt.Fprintf(h, "images.lock\x001\x00%d\x00", len(imagesLock))
	h.Write(imagesLock)
	h.Write([]byte{0})
	fmt.Fprintf(h, "runtime-image-content-id\x001\x00%d\x00", len(runtimeImage))
	h.Write([]byte(runtimeImage))
	h.Write([]byte{0})
	return hex.EncodeToString(h.Sum(nil))
}

func writeLegacyRevisionFile(w io.Writer, name string, present bool, data []byte) {
	presence := byte(0)
	if present {
		presence = 1
	}
	fmt.Fprintf(w, "%s\x00%d\x00%d\x00", name, presence, len(data))
	if present {
		_, _ = w.Write(data)
	}
	_, _ = w.Write([]byte{0})
}

func runtimeArtifactRef(data []byte) ArtifactRef {
	h := sha256.New()
	h.Write([]byte(runtimeArtifactDigestDomain))
	var schema [2]byte
	binary.BigEndian.PutUint16(schema[:], ArtifactDigestSchemaV1)
	h.Write(schema[:])
	h.Write(data)
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return ArtifactRef{
		DigestSchema: ArtifactDigestSchemaV1,
		Digest:       digest,
		MediaType:    RuntimeArtifactMediaTypeV1,
		Size:         int64(len(data)),
	}
}

func validateRuntimeArtifactRef(ref ArtifactRef, data []byte) error {
	if ref.DigestSchema != ArtifactDigestSchemaV1 {
		return invalidArtifactf("unknown digest schema %d", ref.DigestSchema)
	}
	if ref.MediaType != RuntimeArtifactMediaTypeV1 {
		return invalidArtifactf("unknown media type %q", ref.MediaType)
	}
	if ref.Size < 0 || ref.Size != int64(len(data)) {
		return invalidArtifactf("artifact size is %d, got %d bytes", ref.Size, len(data))
	}
	want := runtimeArtifactRef(data)
	if !bytes.Equal(ref.Digest[:], want.Digest[:]) {
		return invalidArtifactf("artifact digest mismatch")
	}
	return nil
}

func encodeProblemProjection(problem models.Problem) ([]byte, error) {
	if problem.CatalogActive || !problem.CreatedAt.IsZero() || !problem.UpdatedAt.IsZero() {
		return nil, invalidArtifactf("catalog state and timestamps are prohibited in the runtime projection")
	}
	if len(problem.Choices) > maxArtifactChoiceCount {
		return nil, invalidArtifactf("problem has too many choices")
	}
	var out bytes.Buffer
	writeUint16(&out, problemProjectionSchemaV1)
	writeUint16(&out, problemProjectionFieldCountV1)
	writeProjectionString(&out, problem.ID)
	writeProjectionString(&out, problem.Revision)
	writeProjectionString(&out, problem.Title)
	writeProjectionString(&out, problem.Description)
	writeProjectionString(&out, problem.Category)
	writeProjectionString(&out, problem.Difficulty)
	writeProjectionString(&out, problem.Type)
	writeUint64(&out, uint64(int64(problem.TimeoutMinutes)))
	writeProjectionString(&out, problem.VerifyType)
	writeProjectionString(&out, problem.BaseImage)
	writeProjectionString(&out, problem.Image)
	writeUint32(&out, uint32(len(problem.Choices)))
	for _, choice := range problem.Choices {
		writeProjectionString(&out, choice.ID)
		writeProjectionString(&out, choice.Text)
	}
	writeProjectionString(&out, problem.CorrectChoice)
	writeProjectionString(&out, problem.Hint)
	writeProjectionString(&out, problem.GradingPrompt)
	if out.Len() > maxProblemProjectionBytes {
		return nil, invalidArtifactf("problem projection exceeds the %d byte limit", maxProblemProjectionBytes)
	}
	return append([]byte(nil), out.Bytes()...), nil
}

func decodeProblemProjection(data []byte) (models.Problem, error) {
	r := bytes.NewReader(data)
	schema, err := readUint16(r)
	if err != nil {
		return models.Problem{}, invalidArtifactWrap("read problem projection schema", err)
	}
	if schema != problemProjectionSchemaV1 {
		return models.Problem{}, invalidArtifactf("unknown problem projection schema %d", schema)
	}
	fieldCount, err := readUint16(r)
	if err != nil {
		return models.Problem{}, invalidArtifactWrap("read problem projection field count", err)
	}
	if fieldCount != problemProjectionFieldCountV1 {
		return models.Problem{}, invalidArtifactf("problem projection field count is %d, want %d", fieldCount, problemProjectionFieldCountV1)
	}

	var problem models.Problem
	if problem.ID, err = readProjectionString(r, maxArtifactIdentityBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.Revision, err = readProjectionString(r, sha256.Size*2); err != nil {
		return models.Problem{}, err
	}
	if problem.Title, err = readProjectionString(r, maxProblemManifestBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.Description, err = readProjectionString(r, maxProblemManifestBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.Category, err = readProjectionString(r, maxArtifactIdentityBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.Difficulty, err = readProjectionString(r, maxArtifactIdentityBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.Type, err = readProjectionString(r, maxArtifactIdentityBytes); err != nil {
		return models.Problem{}, err
	}
	timeout, err := readUint64(r)
	if err != nil {
		return models.Problem{}, invalidArtifactWrap("read timeout", err)
	}
	signedTimeout := int64(timeout)
	if int64(int(signedTimeout)) != signedTimeout {
		return models.Problem{}, invalidArtifactf("timeout does not fit platform int")
	}
	problem.TimeoutMinutes = int(signedTimeout)
	if problem.VerifyType, err = readProjectionString(r, maxArtifactIdentityBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.BaseImage, err = readProjectionString(r, maxProblemManifestBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.Image, err = readProjectionString(r, maxProblemManifestBytes); err != nil {
		return models.Problem{}, err
	}
	choiceCount, err := readUint32(r)
	if err != nil {
		return models.Problem{}, invalidArtifactWrap("read choice count", err)
	}
	// Every choice needs at least two uint64 length prefixes even when both
	// strings are empty. Check the remaining record before allocating so a
	// compact hostile input cannot trigger a disproportionate slice allocation.
	const minimumEncodedChoiceBytes = 2 * 8
	if choiceCount > maxArtifactChoiceCount || uint64(choiceCount) > uint64(r.Len()/minimumEncodedChoiceBytes) {
		return models.Problem{}, invalidArtifactf("problem has too many choices")
	}
	if choiceCount > 0 {
		problem.Choices = make([]models.Choice, int(choiceCount))
		for i := range problem.Choices {
			if problem.Choices[i].ID, err = readProjectionString(r, maxProblemManifestBytes); err != nil {
				return models.Problem{}, err
			}
			if problem.Choices[i].Text, err = readProjectionString(r, maxProblemManifestBytes); err != nil {
				return models.Problem{}, err
			}
		}
	}
	if problem.CorrectChoice, err = readProjectionString(r, maxProblemManifestBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.Hint, err = readProjectionString(r, maxProblemHintBytes); err != nil {
		return models.Problem{}, err
	}
	if problem.GradingPrompt, err = readProjectionString(r, maxProblemManifestBytes); err != nil {
		return models.Problem{}, err
	}
	if r.Len() != 0 {
		return models.Problem{}, invalidArtifactf("trailing data in problem projection")
	}
	return problem, nil
}

func runtimeProblemProjectionEqual(left, right models.Problem) bool {
	leftChoices, rightChoices := left.Choices, right.Choices
	left.Choices, right.Choices = nil, nil
	if !reflect.DeepEqual(left, right) || len(leftChoices) != len(rightChoices) {
		return false
	}
	for i := range leftChoices {
		if leftChoices[i] != rightChoices[i] {
			return false
		}
	}
	return true
}

func captureArtifactOptional(files map[string][]byte, name string) ArtifactOptionalFile {
	data, exists := files[name]
	if !exists || data == nil {
		return ArtifactOptionalFile{}
	}
	return ArtifactOptionalFile{Present: true, Data: cloneArtifactBytes(data)}
}

func validateArtifactOptional(name string, file ArtifactOptionalFile, maxBytes int) error {
	if !file.Present && len(file.Data) != 0 {
		return invalidArtifactf("missing %s carries data", name)
	}
	if len(file.Data) > maxBytes {
		return invalidArtifactf("%s exceeds the %d byte limit", name, maxBytes)
	}
	return nil
}

func artifactOptionalBytes(file ArtifactOptionalFile) []byte {
	if !file.Present {
		return nil
	}
	return cloneArtifactBytes(file.Data)
}

func encodeArtifactOptional(file ArtifactOptionalFile) []byte {
	data := make([]byte, 1+len(file.Data))
	if file.Present {
		data[0] = 1
	}
	copy(data[1:], file.Data)
	return data
}

func decodeArtifactOptional(name string, data []byte) (ArtifactOptionalFile, error) {
	if len(data) == 0 {
		return ArtifactOptionalFile{}, invalidArtifactf("%s optional record is empty", name)
	}
	switch data[0] {
	case 0:
		if len(data) != 1 {
			return ArtifactOptionalFile{}, invalidArtifactf("missing %s carries data", name)
		}
		return ArtifactOptionalFile{}, nil
	case 1:
		return ArtifactOptionalFile{Present: true, Data: cloneArtifactBytes(data[1:])}, nil
	default:
		return ArtifactOptionalFile{}, invalidArtifactf("%s has invalid presence marker %d", name, data[0])
	}
}

func cloneCapturedRuntimeArtifact(artifact CapturedRuntimeArtifact) CapturedRuntimeArtifact {
	copy := artifact
	copy.Manifest = cloneArtifactBytes(artifact.Manifest)
	copy.Setup.Data = cloneArtifactBytes(artifact.Setup.Data)
	copy.Verify.Data = cloneArtifactBytes(artifact.Verify.Data)
	copy.Hint.Data = cloneArtifactBytes(artifact.Hint.Data)
	copy.ImagesLock = cloneArtifactBytes(artifact.ImagesLock)
	copy.Problem = cloneProblem(artifact.Problem)
	return copy
}

func cloneArtifactBytes(data []byte) []byte {
	if data == nil {
		return nil
	}
	cloned := make([]byte, len(data))
	copy(cloned, data)
	return cloned
}

func writeArtifactRecord(w io.Writer, tag uint16, data []byte) {
	writeUint16(w, tag)
	writeUint64(w, uint64(len(data)))
	_, _ = w.Write(data)
}

func readArtifactRecord(r *bytes.Reader, expectedTag uint16, maxBytes int) ([]byte, error) {
	tag, err := readUint16(r)
	if err != nil {
		return nil, invalidArtifactWrap(fmt.Sprintf("read record %d tag", expectedTag), err)
	}
	if tag != expectedTag {
		return nil, invalidArtifactf("record tag is %d, want %d", tag, expectedTag)
	}
	length, err := readUint64(r)
	if err != nil {
		return nil, invalidArtifactWrap(fmt.Sprintf("read record %d length", expectedTag), err)
	}
	if length > uint64(maxBytes) || length > uint64(r.Len()) || length > uint64(math.MaxInt) {
		return nil, invalidArtifactf("record %d length %d exceeds its bound or remaining data", expectedTag, length)
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, invalidArtifactWrap(fmt.Sprintf("read record %d data", expectedTag), err)
	}
	return data, nil
}

func writeProjectionString(w io.Writer, value string) {
	writeUint64(w, uint64(len(value)))
	_, _ = io.WriteString(w, value)
}

func readProjectionString(r *bytes.Reader, maxBytes int) (string, error) {
	length, err := readUint64(r)
	if err != nil {
		return "", invalidArtifactWrap("read projection string length", err)
	}
	if length > uint64(maxBytes) || length > uint64(r.Len()) || length > uint64(math.MaxInt) {
		return "", invalidArtifactf("projection string length %d exceeds its bound or remaining data", length)
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(r, data); err != nil {
		return "", invalidArtifactWrap("read projection string", err)
	}
	return string(data), nil
}

func writeUint16(w io.Writer, value uint16) {
	var data [2]byte
	binary.BigEndian.PutUint16(data[:], value)
	_, _ = w.Write(data[:])
}

func writeUint32(w io.Writer, value uint32) {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	_, _ = w.Write(data[:])
}

func writeUint64(w io.Writer, value uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	_, _ = w.Write(data[:])
}

func readUint16(r io.Reader) (uint16, error) {
	var data [2]byte
	if _, err := io.ReadFull(r, data[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(data[:]), nil
}

func readUint32(r io.Reader) (uint32, error) {
	var data [4]byte
	if _, err := io.ReadFull(r, data[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(data[:]), nil
}

func readUint64(r io.Reader) (uint64, error) {
	var data [8]byte
	if _, err := io.ReadFull(r, data[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(data[:]), nil
}

func invalidArtifactf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRuntimeArtifact, fmt.Sprintf(format, args...))
}

func invalidArtifactWrap(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrInvalidRuntimeArtifact, operation, err)
}
