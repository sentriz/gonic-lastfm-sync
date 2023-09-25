package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
	"github.com/peterbourgon/ff"

	"go.senan.xyz/gonic"
	"go.senan.xyz/gonic/db"
	"go.senan.xyz/gonic/scrobble/lastfm"
)

func main() {
	// re-use as many as the same flags as we can from gonic's main()
	set := flag.NewFlagSet(gonic.Name, flag.ExitOnError)

	confDBPath := set.String("db-path", "", "path to database")
	confGonicUsername := set.String("gonic-username", "", "gonic username for syncing")
	confLastfmUsername := set.String("lastfm-username", "", "lastfm username for syncing")

	if err := ff.Parse(set, os.Args[1:],
		ff.WithConfigFileFlag("config-path"),
		ff.WithConfigFileParser(ff.PlainParser),
		ff.WithEnvVarPrefix(gonic.NameUpper),
	); err != nil {
		log.Panicf("error parsing args: %v\n", err)
	}

	if *confDBPath == "" {
		log.Panicf("please provide a db path")
	}
	if *confGonicUsername == "" {
		log.Panicf("please provide a gonic username")
	}
	if *confLastfmUsername == "" {
		log.Panicf("please provide a lastfm username")
	}

	dbc, err := db.New(*confDBPath, db.DefaultOptions())
	if err != nil {
		log.Panicf("error opening database: %v\n", err)
	}

	apiKey, err := dbc.GetSetting(db.LastFMAPIKey)
	if err != nil {
		log.Panicf("error getting lastfm api key: %v\n", err)
	}
	if apiKey == "" {
		log.Panicf("gonic db doesn't not have a valid lastfm api key")
	}

	client := lastfm.NewClient()

	if err := syncStarsLastFMGonic(client, apiKey, dbc, *confLastfmUsername, *confGonicUsername); err != nil {
		log.Panicf("sync stars from lastfm to gonic: %v", err)
	}

	if err := syncStarsGonicLastFM(client, apiKey, dbc, *confLastfmUsername, *confGonicUsername); err != nil {
		log.Panicf("sync stars from lastfm to gonic: %v", err)
	}
}

func syncStarsLastFMGonic(client *lastfm.Client, apiKey string, dbc *db.DB, lastfmUsername, gonicUsername string) error {
	var user db.User
	if err := dbc.Find(&user, "name=?", gonicUsername).Error; err != nil {
		log.Panicf("error finding gonic user %q: %v\n", gonicUsername, err)
	}

	resp, err := client.UserGetLovedTracks(apiKey, lastfmUsername)
	if err != nil {
		return fmt.Errorf("get loved tracks from lastfm: %v", err)
	}

	for _, starredTrack := range resp.Tracks {
		q := dbc.
			Joins("JOIN album_artists ON album_artists.album_id=tracks.album_id").
			Joins("JOIN artists ON artists.id=album_artists.artist_id").
			Where("tracks.tag_title=? AND artists.name=?", starredTrack.Name, starredTrack.Artist.Name)

		var track db.Track
		if err := q.Find(&track).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("find track in db: %v", err)
		}
		if track.ID == 0 {
			continue
		}

		var star db.TrackStar
		star.UserID = user.ID
		star.TrackID = track.ID

		if err := dbc.Save(&star).Error; err != nil {
			return fmt.Errorf("save track star with user %d track %d: %v", user.ID, track.ID, err)
		}
	}

	return nil
}

func syncStarsGonicLastFM(client *lastfm.Client, apiKey string, dbc *db.DB, lastfmUsername, gonicUsername string) error {
	return nil // TODO: this
}
