package mesh

import "testing"

// FuzzUnmarshalEnvelope feeds arbitrary bytes to the mesh wire-envelope decoder.
// Every gossipsub / stream message arrives as attacker-influenceable bytes and
// is decoded here (codec.go) before dispatch, so the decoder must never panic on
// hostile input; and any envelope that decodes cleanly must survive a re-encode
// / re-decode round-trip unchanged (a decode that silently corrupts or a
// re-encode that can't reproduce its own input would be a wire-corruption bug).
// Seeded with one valid envelope plus malformed JSON.
func FuzzUnmarshalEnvelope(f *testing.F) {
	seed, err := marshalEnvelope(envelope{Key: "cerberus/telemetry/x", Payload: []byte("hello")})
	if err != nil {
		f.Fatalf("seed marshal: %v", err)
	}
	f.Add(seed)
	f.Add([]byte(nil))
	f.Add([]byte("{"))
	f.Add([]byte(`{"Key":123}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var e envelope
		if err := unmarshalEnvelope(data, &e); err != nil {
			return // rejecting malformed input is correct; we only require no panic
		}
		reencoded, err := marshalEnvelope(e)
		if err != nil {
			t.Fatalf("re-marshal of a decoded envelope failed: %v", err)
		}
		var e2 envelope
		if err := unmarshalEnvelope(reencoded, &e2); err != nil {
			t.Fatalf("re-unmarshal of a re-encoded envelope failed: %v", err)
		}
		if e2.Key != e.Key || string(e2.Payload) != string(e.Payload) {
			t.Fatalf("round-trip mismatch: got %+v want %+v", e2, e)
		}
	})
}
