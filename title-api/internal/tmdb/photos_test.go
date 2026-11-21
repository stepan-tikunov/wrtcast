package tmdb

import (
	"context"
	"reflect"
	"testing"
)

func TestPhotos(t *testing.T) {
	fake := newFakeTMDB(t)
	service, _ := newServingService(t, fake, "key")
	ctx := context.Background()
	photos, err := service.Photos(ctx, 819)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"best.jpg", "mid.jpg", "low.jpg"}; !reflect.DeepEqual(photos.Profiles, want) {
		t.Errorf("profiles %v, want %v", photos.Profiles, want)
	}
	want := []TaggedPhoto{
		{File: "still.jpg", Kind: KindTV, MediaID: 1405, Type: "still", Width: 1280, Height: 720},
		{File: "backdrop.jpg", Kind: KindMovie, MediaID: 550, Type: "backdrop", Width: 1920, Height: 1080},
		{File: "poster.jpg", Kind: KindMovie, MediaID: 550, Type: "poster", Width: 1000, Height: 1500},
	}
	if !reflect.DeepEqual(photos.Tagged, want) {
		t.Errorf("tagged %+v, want %+v", photos.Tagged, want)
	}
	if _, err := service.Photos(ctx, 819); err != nil || fake.hits["/person/819/images"] != 1 {
		t.Errorf("cached: %v, %d requests", err, fake.hits["/person/819/images"])
	}
	if _, err := service.Photos(ctx, 1); err != ErrNotFound {
		t.Errorf("someone TMDB doesn't have: %v", err)
	}
}

func TestEpisodeCast(t *testing.T) {
	fake := newFakeTMDB(t)
	service, _ := newServingService(t, fake, "key")
	ctx := context.Background()
	people, err := service.EpisodeCast(ctx, 1405, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []Person{{ID: 17414, Name: "Майкл Си. Холл", Character: "Dexter Morgan", Profile: "hall.jpg"}, {ID: 555, Name: "Гость", Character: "Сосед", Profile: "guest.jpg"}, {ID: 556, Name: "Без фото"}}
	if !reflect.DeepEqual(people, want) {
		t.Errorf("people %+v, want %+v", people, want)
	}
	if people, err := service.EpisodeCast(ctx, 1405, 1, 3); err != nil || len(people) != 0 {
		t.Errorf("an episode TMDB doesn't have: %v %v", people, err)
	}
}
