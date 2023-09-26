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

var (
	set = flag.NewFlagSet(gonic.Name, flag.ExitOnError)

	// re-use as many as the same flags as we can from gonic's main()
	confDBPath         = set.String("db-path", "", "path to database")
	confGonicUsername  = set.String("gonic-username", "", "gonic username for syncing")
	confLastfmUsername = set.String("lastfm-username", "", "lastfm username for syncing")
)

func main() {
	if err := ff.Parse(set, os.Args[1:],
		ff.WithConfigFileFlag("config-path"),
		ff.WithConfigFileParser(ff.PlainParser),
		ff.WithEnvVarPrefix(gonic.NameUpper),
	); err != nil {
		log.Panicf("error parsing args: %v\n", err)
	}

	if *confDBPath == "" {
		log.Fatalf("please provide a db path")
	}
	if *confGonicUsername == "" {
		log.Fatalf("please provide a gonic username")
	}
	if *confLastfmUsername == "" {
		log.Fatalf("please provide a lastfm username")
	}

	dbc, err := db.New(*confDBPath, db.DefaultOptions())
	if err != nil {
		log.Panicf("error opening database: %v\n", err)
	}
	defer dbc.Close()

	if err := dbc.AutoMigrate(&LastFMSyncUploadedTrack{}).Error; err != nil {
		log.Panicf("migrate db: %v", err)
	}

	apiKey, err := dbc.GetSetting(db.LastFMAPIKey)
	if err != nil {
		log.Panicf("error getting lastfm api key: %v\n", err)
	}
	if apiKey == "" {
		log.Panicf("gonic db doesn't not have a valid lastfm api key")
	}

	if err := syncStarsLastFMToGonic(apiKey, dbc, *confLastfmUsername, *confGonicUsername); err != nil {
		log.Panicf("sync stars from lastfm to gonic: %v", err)
	}

	if err := syncStarsGonicToLastFM(apiKey, dbc, *confLastfmUsername, *confGonicUsername); err != nil {
		log.Panicf("sync stars from gonic to lastfm: %v", err)
	}
}

var (
	searchFtExpr         = regexp.MustCompile(`\b(ft\.|featuring|feat|feat\.)\s+.*$`)
	searchPuncExpr       = regexp.MustCompile(`[^a-zA-Z0-9\\p{L}\\p{N}]`)
	searchConcatReplacer = strings.NewReplacer(
		" and ", "", " & ", "",
		" vs. ", "", " vs ", "",
		" his ", "",
		" the ", "",
		" with ", "",
	)
)

func searchKey(artist, track string) string {
	artist = strings.ToLower(artist)
	track = strings.ToLower(track)
	artist = searchFtExpr.ReplaceAllString(artist, "")
	track = searchFtExpr.ReplaceAllString(track, "")
	key := artist + track
	key = searchConcatReplacer.Replace(key)
	key = searchPuncExpr.ReplaceAllString(key, "")
	return key
}

func syncStarsLastFMToGonic(apiKey string, dbc *db.DB, lastfmUsername, gonicUsername string) error {
	var user db.User
	if err := dbc.Find(&user, "name=?", gonicUsername).Error; err != nil {
		return fmt.Errorf("find gonic user %q: %w\n", gonicUsername, err)
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
		searchStrings = append(searchStrings, searchKey(track.TagTrackArtist, track.TagTitle))
	}

	var saved int
	for _, starred := range resp.Tracks {
		query := searchKey(starred.Artist.Name, starred.Name)
		ranks := fuzzy.RankFindNormalized(query, searchStrings)
		if len(ranks) == 0 {
			log.Printf("no match for %q", query)
			continue
		}
		sort.Sort(ranks)

		track := tracks[ranks[0].OriginalIndex]
		starDateUTS, _ := strconv.Atoi(starred.Date.UTS)

		if err := dbc.Save(&db.TrackStar{UserID: user.ID, TrackID: track.ID, StarDate: time.Unix(int64(starDateUTS), 0)}).Error; err != nil {
			return fmt.Errorf("save track star with user %d track %d: %v", user.ID, track.ID, err)
		}

		if err := dbc.Save(&LastFMSyncUploadedTrack{UserID: user.ID, TrackID: track.ID}).Error; err != nil {
			return fmt.Errorf("save lastfm sync uploaded track: %v", err)
		}

		saved++
	}

	log.Printf("saved lastfm->gonic stars, %d of %d matched", saved, len(resp.Tracks))

	return nil
}

func syncStarsGonicToLastFM(apiKey string, dbc *db.DB, lastfmUsername, gonicUsername string) error {
	var user db.User
	if err := dbc.Find(&user, "name=?", gonicUsername).Error; err != nil {
		return fmt.Errorf("find gonic user %q: %w\n", gonicUsername, err)
	}

	q := dbc.
		Preload("TrackStar").
		Joins("JOIN track_stars ON track_stars.track_id=tracks.id").
		Joins("LEFT JOIN last_fm_sync_uploaded_tracks ON last_fm_sync_uploaded_tracks.user_id=track_stars.user_id AND last_fm_sync_uploaded_tracks.track_id=track_stars.track_id").
		Where("track_stars.user_id=?", user.ID).
		Where("last_fm_sync_uploaded_tracks.track_id IS NULL")

	var tracks []*db.Track
	if err := q.Find(&tracks).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("find local stars: %w", err)
	}

	client := lastfm.NewClient()
	scrobbler := lastfm.NewScrobbler(dbc, client)

	for _, track := range tracks {
		if err := scrobbler.LoveTrack(&user, track); err != nil {
			return fmt.Errorf("loving lastfm track: %w", err)
		}

		if err := dbc.Save(&LastFMSyncUploadedTrack{UserID: user.ID, TrackID: track.ID}).Error; err != nil {
			return fmt.Errorf("save lastfm sync uploaded track: %v", err)
		}
	}

	log.Printf("saved gonic->lastfm stars, %d new", len(tracks))

	return nil
}

type LastFMSyncUploadedTrack struct {
	UserID  int `gorm:"primary_key; not null" sql:"default: null; type:int REFERENCES users(id) ON DELETE CASCADE"`
	TrackID int `gorm:"primary_key; not null" sql:"default: null; type:int REFERENCES tracks(id) ON DELETE CASCADE"`
}
