package blog

import (
	"context"
	"testing"
	"time"
)

func TestDatabasePostRoundTrip(t *testing.T) {
	a := testApp(t)
	p := seedPost(t, a, "roundtrip", true, false, time.Now().Add(-time.Hour))
	got, err := a.Store.PostID(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != p.Title || got.PublishedAt.Unix() != p.PublishedAt.Unix() {
		t.Fatalf("roundtrip: %+v", got)
	}
}
