package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The primary player is down (the provider's embed host answers 422), so the
// media type and the episode list must come from the alternative player.
func TestAlternativePlayerFallback(t *testing.T) {
	t.Parallel()

	catalogs := map[string]string{
		"aaa111": `{"type":"serial","all":{"1":{"1":{"t1":{"id":11,"translation":"Dub"}},"2":{"t1":{"id":12,"translation":"Dub"}}},"x":{"1":{}}}}`,
		"bbb222": `{"type":"movie","active":{"id":7,"translation":"Dub"},"all":{"directors":["x"]}}`,
	}
	player := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = fmt.Fprintf(response, `<meta name="viewporti" content="abc">
<script>const userParam = { token: 'tok' }; const fileList = JSON.parse('%s');</script>`,
			catalogs[request.URL.Query().Get("token_movie")])
	}))
	defer player.Close()

	var siteURL string
	site := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/index.php":
			movie := map[string]string{"20973": "aaa111", "42185": "bbb222"}[request.URL.Query().Get("newsid")]
			_, _ = fmt.Fprintf(response, `<iframe src="%s/embed/movie/378"></iframe>
<iframe src="%s/?token_movie=%s&amp;token=def456"></iframe>`, siteURL, player.URL, movie)
		default:
			response.WriteHeader(http.StatusUnprocessableEntity)
		}
	}))
	defer site.Close()
	siteURL = site.URL

	client, err := NewClient(Config{BaseURL: site.URL, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	seriesID := client.ExternalTitleID("20973")
	_, page, _, err := client.movieDocument(ctx, seriesID)
	if err != nil {
		t.Fatal(err)
	}
	if mediaType, err := client.mediaTypeFromAlternativePlayers(ctx, page); err != nil || mediaType != MediaTypeSeries {
		t.Fatalf("series media type = %v, %v", mediaType, err)
	}
	seasons, err := client.Series(ctx, seriesID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 1 || seasons[0].Number != 1 || len(seasons[0].Episodes) != 2 ||
		seasons[0].Episodes[0].Number != "1" || seasons[0].Episodes[1].Number != "2" {
		t.Fatalf("unexpected seasons: %+v", seasons)
	}

	_, page, _, err = client.movieDocument(ctx, client.ExternalTitleID("42185"))
	if err != nil {
		t.Fatal(err)
	}
	if mediaType, err := client.mediaTypeFromAlternativePlayers(ctx, page); err != nil || mediaType != MediaTypeMovie {
		t.Fatalf("movie media type = %v, %v", mediaType, err)
	}
}
