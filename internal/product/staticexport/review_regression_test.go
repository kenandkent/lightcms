package staticexport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReviewJSONLDDoesNotRewritePublisherIdentity(t *testing.T) {
	input := []byte(`{"@type":"NewsArticle","url":"https://cms.test/a","publisher":{"@type":"Organization","url":"https://publisher.test","@id":"https://publisher.test/#brand"},"author":{"@type":"Person","url":"https://author.test"},"mainEntityOfPage":{"@id":"https://cms.test/a"}}`)
	out, e := transformJSONLD(input, func(r Reference) (string, error) {
		if r.Kind == "page" {
			return "https://worker.test/a", nil
		}
		return r.Value, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	var value map[string]any
	json.Unmarshal(out, &value)
	if value["publisher"].(map[string]any)["url"] != "https://publisher.test" || value["author"].(map[string]any)["url"] != "https://author.test" || value["url"] != "https://worker.test/a" {
		t.Fatal(string(out))
	}
}
func TestReviewJSONLDRejectsTrailingJSON(t *testing.T) {
	if _, e := transformJSONLD([]byte(`{"url":"a"}{"url":"b"}`), func(r Reference) (string, error) { return r.Value, nil }); e == nil {
		t.Fatal("multiple JSON values accepted")
	}
}
func TestReviewSrcsetCandidateSeparators(t *testing.T) {
	var refs []string
	out, e := transformSrcset("/a.png, /b.png 2x", func(ref string) (string, error) { refs = append(refs, ref); return "/hashed" + ref, nil })
	if e != nil || len(refs) != 2 || refs[0] != "/a.png" || refs[1] != "/b.png" || !strings.Contains(out, "/hashed/a.png") {
		t.Fatalf("refs=%v output=%s err=%v", refs, out, e)
	}
}
