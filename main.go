package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
	"github.com/lithammer/fuzzysearch/fuzzy"
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
	confMinStarDate := set.Uint("min-star-date", 0, "a unix timestamp (sec) past which gonic->lastfm stars wont be send (the date that you set gonic-lastfm-sync up for the first time)")

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
	if *confMinStarDate == 0 {
		log.Panicf("please provide a min star date")
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

	if err := syncStarsLastFMGonic(apiKey, dbc, *confLastfmUsername, *confGonicUsername); err != nil {
		log.Panicf("sync stars from lastfm to gonic: %v", err)
	}

	minStarDate := time.Unix(int64(*confMinStarDate), 0)
	if err := syncStarsGonicLastFM(apiKey, dbc, *confLastfmUsername, *confGonicUsername, minStarDate); err != nil {
		log.Panicf("sync stars from gonic to lastfm: %v", err)
	}
}

var searchPuncExpr = regexp.MustCompile(`[^a-zA-Z0-9\\p{L}\\p{N}]`)
var searchReplacer = strings.NewReplacer(
	" and ", "", " & ", "",
	" feat. ", "", " feat ", "", " featuring ", "", " ft. ", "",
	" vs. ", "", " vs ", "",
	" his ", "",
	" the ", "",
	" with ", "",
)

func transformForSearch(inp string) string {
	inp = strings.ToLower(inp)
	inp = searchReplacer.Replace(inp)
	inp = searchPuncExpr.ReplaceAllString(inp, "")
	return inp
}

func syncStarsLastFMGonic(apiKey string, dbc *db.DB, lastfmUsername, gonicUsername string) error {
	var user db.User
	if err := dbc.Find(&user, "name=?", gonicUsername).Error; err != nil {
		log.Panicf("error finding gonic user %q: %v\n", gonicUsername, err)
	}

	client := lastfm.NewClient()
	resp, err := client.UserGetLovedTracks(apiKey, lastfmUsername)
	if err != nil {
		return fmt.Errorf("get loved tracks from lastfm: %v", err)
	}

	var tracks []*db.Track
	if err := dbc.Select("id, tag_track_artist, tag_title").Find(&tracks).Error; err != nil {
		return fmt.Errorf("list tracks in db: %v", err)
	}

	var searchStrings []string
	for _, track := range tracks {
		searchStrings = append(searchStrings, transformForSearch(track.TagTrackArtist+track.TagTitle))
	}

	var saved int
	for _, starred := range resp.Tracks {
		query := transformForSearch(starred.Artist.Name + starred.Name)
		ranks := fuzzy.RankFindNormalized(query, searchStrings)
		if len(ranks) == 0 {
			log.Printf("no match for %q", query)
			continue
		}
		sort.Sort(ranks)
		track := tracks[ranks[0].OriginalIndex]

		var star db.TrackStar
		star.UserID = user.ID
		star.TrackID = track.ID

		starDateUTS, _ := strconv.Atoi(starred.Date.UTS)
		star.StarDate = time.Unix(int64(starDateUTS), 0)

		if err := dbc.Save(&star).Error; err != nil {
			return fmt.Errorf("save track star with user %d track %d: %v", user.ID, track.ID, err)
		}

		saved++
	}

	log.Printf("saved lastfm->gonic stars %d/%d", saved, len(resp.Tracks))

	return nil
}

func syncStarsGonicLastFM(apiKey string, dbc *db.DB, lastfmUsername, gonicUsername string, minStarDate time.Time) error {
	var user db.User
	if err := dbc.Find(&user, "name=?", gonicUsername).Error; err != nil {
		log.Panicf("error finding gonic user %q: %v\n", gonicUsername, err)
	}

	q := dbc.
		Preload("TrackStar").
		Joins("JOIN track_stars ON track_stars.track_id=tracks.id").
		Where("track_stars.user_id=?", user.ID)

	var tracks []*db.Track
	if err := q.Find(&tracks).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("find local stars: %w", err)
	}

	client := lastfm.NewClient()
	scrobbler := lastfm.NewScrobbler(dbc, client)

	var saved int
	for _, track := range tracks {
		if track.TrackStar.StarDate.Before(minStarDate) {
			continue
		}

		if err := scrobbler.LoveTrack(&user, track); err != nil {
			return fmt.Errorf("loving lastfm track: %w", err)
		}

		saved++
	}

	log.Printf("saved gonic->lastfm stars %d/%d", saved, len(tracks))

	return nil
}
