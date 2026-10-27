package archive

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"

	"amlogicmini/internal/profile"
)

const (
	SchemaVersion = 2
	LockFileName  = ".amlogic-mini.lock"
)

type File struct {
	Name       string `json:"name"`
	Size       int64  `json:"bytes"`
	Expected   int64  `json:"expected_bytes"`
	SHA256     string `json:"sha256"`
	Status     string `json:"status"`
	DoubleRead bool   `json:"double_read_verified"`
	Error      string `json:"read_error,omitempty"`
}

type Chunk struct {
	Index             int       `json:"index"`
	NANDOffset        uint32    `json:"nand_offset"`
	FileOffset        int64     `json:"file_offset"`
	Size              uint32    `json:"bytes"`
	Hashes            [2]string `json:"sha256"`
	DifferentBytes    int       `json:"different_bytes"`
	DifferentBits     int       `json:"different_bits"`
	Offsets           []uint32  `json:"difference_offsets,omitempty"`
	OffsetsTruncated  bool      `json:"difference_offsets_truncated,omitempty"`
	TransportVerified bool      `json:"transport_verified"`
}

type Manifest struct {
	Schema      int             `json:"schema"`
	Version     string          `json:"tool_version"`
	Profile     profile.Profile `json:"profile"`
	Identity    string          `json:"device_fingerprint"`
	Firmware    [7]byte         `json:"firmware"`
	Mode        string          `json:"mode"`
	Status      string          `json:"status"`
	Started     string          `json:"started_utc"`
	Finished    string          `json:"finished_utc,omitempty"`
	Files       []File          `json:"files"`
	Chunks      []Chunk         `json:"raw_chunks,omitempty"`
	Warnings    []string        `json:"warnings,omitempty"`
	Error       string          `json:"error,omitempty"`
	ResumeCount int             `json:"resume_count,omitempty"`
}

var safeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func SafeName(name string) bool {
	return safeNamePattern.MatchString(name) && name != "." && name != ".."
}

func ExpectedFiles(p profile.Profile, mode string) map[string]int64 {
	files := map[string]int64{}
	for _, r := range p.Reserved {
		if r.Name == "keys" {
			files["rsv-keys.bin"] = int64(r.Size)
		}
	}
	if mode != "raw" {
		files["env.raw.bin"] = 65536
		files["env.txt"] = 0
		for _, r := range p.Partitions {
			files[r.Name+".bin"] = int64(r.Size)
		}
		for _, r := range p.Reserved {
			files["rsv-"+r.Name+".bin"] = int64(r.Size)
		}
	}
	if mode != "partitions" {
		size := int64(p.Geometry.RawBytes(p.Geometry.NANDSize))
		files["nand-full-read1.raw.bin"] = size
		files["nand-full-read2.raw.bin"] = size
	}
	return files
}

func validate(m *Manifest) error {
	if m.Schema != SchemaVersion {
		return fmt.Errorf("unsupported manifest schema %d", m.Schema)
	}
	if err := m.Profile.Validate(); err != nil {
		return err
	}
	if known, ok := profile.Lookup(m.Profile.Name); ok && !reflect.DeepEqual(known, m.Profile) {
		return fmt.Errorf("stored profile does not match the supported %s layout", m.Profile.Name)
	}
	if m.ResumeCount < 0 {
		return fmt.Errorf("invalid resume counter")
	}
	if m.Mode != "all" && m.Mode != "partitions" && m.Mode != "raw" {
		return fmt.Errorf("invalid backup mode %q", m.Mode)
	}
	if len(m.Identity) != 64 && !(m.Identity == "" && len(m.Files) == 0 && len(m.Chunks) == 0 && m.Status != "complete" && m.Status != "warnings") {
		return fmt.Errorf("missing device fingerprint")
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if !SafeName(f.Name) || seen[f.Name] {
			return fmt.Errorf("invalid or duplicate archive filename %q", f.Name)
		}
		seen[f.Name] = true
		if f.Size <= 0 || f.Expected < f.Size || len(f.SHA256) != 64 {
			return fmt.Errorf("invalid file metadata for %s", f.Name)
		}
		if f.Status != "verified" && f.Status != "partial" && f.Status != "captured" {
			return fmt.Errorf("invalid file status for %s", f.Name)
		}
		if f.Status != "partial" && f.Size != f.Expected {
			return fmt.Errorf("incomplete file marked complete: %s", f.Name)
		}
	}
	return nil
}

func Save(root string, m *Manifest) error {
	if err := validate(m); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return ReplaceFile(root, "manifest.json", append(data, '\n'))
}

func Load(root string) (*Manifest, error) {
	file, err := openRegular(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<20))
	decoder.DisallowUnknownFields()
	var m Manifest
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("manifest contains trailing data")
	}
	if err := validate(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// EnsureRegular refuses symlinks as well as directories and device nodes.
func EnsureRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular backup file: %s", filepath.Base(path))
	}
	return nil
}
