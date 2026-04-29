package gatews

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sbeSchema is a partial unmarshal target, just enough to validate the
// invariants that downstream consumers care about (package, ids, version,
// counts of messages and fields). We don't replicate the full SBE schema
// here on purpose – this is a sanity test, not a code generator.
type sbeSchema struct {
	XMLName         xml.Name `xml:"messageSchema"`
	Package         string   `xml:"package,attr"`
	ID              int      `xml:"id,attr"`
	Version         int      `xml:"version,attr"`
	SemanticVersion string   `xml:"semanticVersion,attr"`
	ByteOrder       string   `xml:"byteOrder,attr"`
	Types           struct {
		Composites []struct {
			Name string `xml:"name,attr"`
		} `xml:"composite"`
		Enums []struct {
			Name string `xml:"name,attr"`
		} `xml:"enum"`
	} `xml:"types"`
	Messages []struct {
		Name       string `xml:"name,attr"`
		ID         int    `xml:"id,attr"`
		Fields     []struct {
			Name string `xml:"name,attr"`
			ID   int    `xml:"id,attr"`
		} `xml:"field"`
		Data []struct {
			Name string `xml:"name,attr"`
			ID   int    `xml:"id,attr"`
		} `xml:"data"`
		Groups []struct {
			Name string `xml:"name,attr"`
			ID   int    `xml:"id,attr"`
		} `xml:"group"`
	} `xml:"message"`
}

var sbeSchemaPaths = []string{
	"../sbe/schemas/prod/gate_fex_ws_v1.0.0.xml",
	"../sbe/schemas/prod/gate_fex_ws_latest.xml",
	"../sbe/schemas/testnet/gate_fex_ws_v1.0.0.xml",
	"../sbe/schemas/testnet/gate_fex_ws_latest.xml",
}

func loadSBESchema(t *testing.T, path string) (*sbeSchema, []byte) {
	t.Helper()
	abs, _ := filepath.Abs(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("schema not available at %s (skipping): %v", abs, err)
	}
	var s sbeSchema
	if err := xml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema %s is not well-formed XML: %v", path, err)
	}
	return &s, raw
}

// TestSBESchemasAreWellFormedXML verifies each SBE schema file parses as
// well-formed XML and has the basic structure expected by downstream SBE
// code generators.
func TestSBESchemasAreWellFormedXML(t *testing.T) {
	for _, path := range sbeSchemaPaths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			s, _ := loadSBESchema(t, path)

			if s.Package == "" {
				t.Errorf("package attr missing in %s", path)
			}
			if s.ID == 0 {
				t.Errorf("schemaId missing or zero in %s", path)
			}
			if s.Version == 0 {
				t.Errorf("version missing or zero in %s", path)
			}
			if s.ByteOrder == "" {
				t.Errorf("byteOrder missing in %s", path)
			}
			if len(s.Types.Composites) == 0 {
				t.Errorf("no composites declared in %s", path)
			}
			if len(s.Messages) == 0 {
				t.Errorf("no messages declared in %s", path)
			}
		})
	}
}

// TestSBESchemasHaveStableIdentity verifies all four schemas declare the
// same package and schemaId. If prod/testnet ever diverge in identity,
// downstream codegen could silently mis-route messages.
func TestSBESchemasHaveStableIdentity(t *testing.T) {
	if len(sbeSchemaPaths) == 0 {
		t.Skip("no schemas configured")
	}

	first, _ := loadSBESchema(t, sbeSchemaPaths[0])

	for _, path := range sbeSchemaPaths[1:] {
		s, _ := loadSBESchema(t, path)
		if s.Package != first.Package {
			t.Errorf("package mismatch: %s has %q, %s has %q",
				sbeSchemaPaths[0], first.Package, path, s.Package)
		}
		if s.ID != first.ID {
			t.Errorf("schemaId mismatch: %s has %d, %s has %d",
				sbeSchemaPaths[0], first.ID, path, s.ID)
		}
	}
}

// TestSBESchemaMessagesHaveUniqueIDs ensures no two messages share the same
// templateId within a schema (would be ambiguous for SBE decoders).
func TestSBESchemaMessagesHaveUniqueIDs(t *testing.T) {
	for _, path := range sbeSchemaPaths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			s, _ := loadSBESchema(t, path)
			seen := map[int]string{}
			for _, m := range s.Messages {
				if other, exists := seen[m.ID]; exists {
					t.Errorf("duplicate templateId %d in %s: %q and %q",
						m.ID, path, other, m.Name)
				}
				seen[m.ID] = m.Name
			}
		})
	}
}

// TestSBESchemaFieldIDsUniqueWithinMessage ensures each field has a unique id
// within its containing message (SBE wire-decoding requirement).
func TestSBESchemaFieldIDsUniqueWithinMessage(t *testing.T) {
	for _, path := range sbeSchemaPaths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			s, _ := loadSBESchema(t, path)

			for _, m := range s.Messages {
				seen := map[int]string{}
				record := func(kind, name string, id int) {
					if other, ok := seen[id]; ok {
						t.Errorf("duplicate id %d in message %q (%s): %s vs %s %q",
							id, m.Name, path, other, kind, name)
					}
					seen[id] = kind + ":" + name
				}
				for _, f := range m.Fields {
					record("field", f.Name, f.ID)
				}
				for _, g := range m.Groups {
					record("group", g.Name, g.ID)
				}
				for _, d := range m.Data {
					record("data", d.Name, d.ID)
				}
			}
		})
	}
}

// TestSBESchemaSemanticVersionMatchesFilename guards against version drift:
// a file named gate_fex_ws_v1.0.0.xml must declare semanticVersion="1.0.0".
// "latest" files are exempt (they snapshot the current head).
func TestSBESchemaSemanticVersionMatchesFilename(t *testing.T) {
	for _, path := range sbeSchemaPaths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			s, _ := loadSBESchema(t, path)
			base := filepath.Base(path)
			if base == "gate_fex_ws_latest.xml" {
				t.Skipf("latest is a moving snapshot; semanticVersion=%s", s.SemanticVersion)
			}
			// Expect the filename "gate_fex_ws_vX.Y.Z.xml" to encode the version
			// as the SemanticVersion attribute (X.Y.Z).
			expected := strings.TrimSuffix(strings.TrimPrefix(base, "gate_fex_ws_v"), ".xml")
			if s.SemanticVersion != expected {
				t.Errorf("semanticVersion mismatch: file=%s claims %q, filename suggests %q",
					base, s.SemanticVersion, expected)
			}
		})
	}
}

// TestSBESchemaContainsRequiredCompositeTypes verifies that downstream
// codegen-required composite types (messageHeader, varString8,
// groupSize16Encoding) are declared. Their absence would make the
// generated SBE encoder unbuildable.
func TestSBESchemaContainsRequiredCompositeTypes(t *testing.T) {
	required := []string{"messageHeader", "varString8", "groupSize16Encoding"}
	for _, path := range sbeSchemaPaths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			s, _ := loadSBESchema(t, path)
			present := map[string]bool{}
			for _, c := range s.Types.Composites {
				present[c.Name] = true
			}
			for _, want := range required {
				if !present[want] {
					t.Errorf("required composite type %q missing in %s", want, path)
				}
			}
		})
	}
}

// TestSBESchemaByteOrderLittleEndian verifies that all schemas declare
// byteOrder="littleEndian" — the default for SBE on x86 / ARM platforms.
// A mismatch here would silently corrupt wire decoding.
func TestSBESchemaByteOrderLittleEndian(t *testing.T) {
	for _, path := range sbeSchemaPaths {
		s, _ := loadSBESchema(t, path)
		if s.ByteOrder != "littleEndian" {
			t.Errorf("schema %s byteOrder=%q want littleEndian", path, s.ByteOrder)
		}
	}
}

// TestSBESchemaPackageIsKnownIdentifier guards against accidental package
// rename by checking against the expected package value.
func TestSBESchemaPackageIsKnownIdentifier(t *testing.T) {
	const want = "gate_fex_ws_sbe"
	for _, path := range sbeSchemaPaths {
		s, _ := loadSBESchema(t, path)
		if s.Package != want {
			t.Errorf("schema %s package=%q want %q (rename would break codegen import paths)",
				path, s.Package, want)
		}
	}
}

// TestSBEProdAndTestnetByteIdentical documents the current state: as of this
// commit all 4 schema files are byte-identical. If they ever diverge, this
// test will fail loudly so reviewers must explicitly acknowledge the fork.
//
// To intentionally diverge, update this test to compare against expected
// per-environment hashes instead of asserting identity.
func TestSBEProdAndTestnetByteIdentical(t *testing.T) {
	hashes := map[string]string{}
	for _, path := range sbeSchemaPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("schema not available at %s: %v", path, err)
		}
		sum := md5.Sum(raw)
		hashes[path] = hex.EncodeToString(sum[:])
	}

	if len(hashes) < 2 {
		t.Skip("need at least 2 schemas to compare")
	}

	var canonical string
	for _, h := range hashes {
		canonical = h
		break
	}
	for path, h := range hashes {
		if h != canonical {
			t.Logf("schema %s diverged: hash %s vs canonical %s", path, h, canonical)
			t.Logf("if intentional, update TestSBEProdAndTestnetByteIdentical to assert per-env hashes")
			t.Fail()
		}
	}
}
