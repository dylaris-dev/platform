package database

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/store"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIntegrationPackForkOwnershipAndAtomicEmptyTarget(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	ctx := context.Background()
	users := []*models.User{{Username: uniqueName("fork_a_"), Password: "test"}, {Username: uniqueName("fork_b_"), Password: "test"}}
	for _, u := range users {
		u.Email = u.Username + "@example.test"
		if err := st.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	origin := &models.Pack{OwnerID: users[0].ID, InternalName: "Original", InternalSlug: "original", ModrinthVisibility: "unlisted"}
	id, err := st.CreatePack(origin)
	if err != nil {
		t.Fatal(err)
	}
	origin.ID = id
	source := &models.PackBuild{PackID: id, VersionString: "1.0", Minecraft: "1.20.1", Loader: "forge", LoaderVersion: "47.4.28", Channel: "draft", MinJava: "17", MinMemory: 2048, Changelog: "Config notes"}
	source.ID, err = st.CreatePackBuild(source)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := st.UpsertMod(&models.Mod{OwnerID: users[0].ID, Slug: "script", PrettyName: "script.js", ContentType: models.ContentTypeConfig})
	if err != nil {
		t.Fatal(err)
	}
	mv, err := st.CreateModversion(&models.Modversion{ModID: mod, Version: "initial", Filesize: 32, StorageKey: "packs/source-owner/upload.zip", Source: "upload", TargetPath: "kubejs/server_scripts/script.js"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AttachModversionToBuild(source.ID, mv, "both"); err != nil {
		t.Fatal(err)
	}
	if err = st.SetPackFolder(ctx, source.ID, "kubejs/empty/deep", false); err != nil {
		t.Fatal(err)
	}
	content, err := st.ListBuildContent(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	content[0].StorageKey = "packs/target-owner/forks/fixture/script.zip"
	makeFork := func(target *models.Pack, suffix string) *models.PackFork {
		return &models.PackFork{SourcePackID: origin.ID, SourceBuildID: source.ID, Target: target, Build: &models.PackBuild{VersionString: source.VersionString, Minecraft: source.Minecraft, Loader: source.Loader, LoaderVersion: source.LoaderVersion, MinJava: source.MinJava, MinMemory: source.MinMemory, Changelog: source.Changelog}, CatalogSuffix: suffix, Content: content, Folders: []string{"kubejs/empty/deep"}}
	}
	f := makeFork(&models.Pack{OwnerID: users[1].ID, InternalName: "Independent", InternalSlug: "independent"}, "first")
	forkPack, forkBuild, err := st.ForkPackBuild(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.GetPack(forkPack)
	if err != nil || p.OwnerID != users[1].ID || p.ModrinthProjectID != "" || p.RecommendedBuildID != nil {
		t.Fatalf("fork pack binding leaked: %+v %v", p, err)
	}
	b, err := st.GetPackBuild(forkBuild)
	if err != nil || b.Frozen || b.ModrinthPublished || b.MrpackStorageKey != "" || b.Channel != "draft" || b.Changelog != source.Changelog || b.MinMemory != 2048 {
		t.Fatalf("fork publication/source metadata wrong: %+v %v", b, err)
	}
	rows, err := st.ListBuildContent(forkBuild)
	if err != nil || len(rows) != 1 || rows[0].ID == mv || rows[0].ModID == mod || rows[0].StorageKey != content[0].StorageKey || rows[0].Side != "both" {
		t.Fatalf("fork content is not independent: %+v %v", rows, err)
	}
	catalog, err := st.GetModBySlug(users[1].ID, rows[0].ModSlug)
	if err != nil || catalog == nil || catalog.OwnerID != users[1].ID {
		t.Fatal("fork catalog belongs to source owner")
	}
	folders, err := st.ListPackFolders(ctx, forkBuild)
	if err != nil || len(folders) != 1 || folders[0] != "kubejs/empty/deep" {
		t.Fatal("fork lost empty folders")
	}
	unused, err := st.UnreferencedPackForkUploads(ctx, "first", []string{content[0].StorageKey, "packs/target-owner/forks/uncommitted.zip"})
	if err != nil || len(unused) != 1 || unused[0] != "packs/target-owner/forks/uncommitted.zip" {
		t.Fatal("cleanup would delete committed fork files")
	}
	hold, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hold.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "pack-fork:pending"); err != nil {
		hold.Rollback()
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, waitErr := st.UnreferencedPackForkUploads(deadline, "pending", []string{content[0].StorageKey})
	cancel()
	hold.Rollback()
	if waitErr == nil {
		t.Fatal("cleanup did not wait for unresolved fork transaction")
	}
	empty := &models.Pack{OwnerID: users[1].ID, InternalName: "Existing target", InternalSlug: "existing", Summary: "Keep settings", ModrinthVisibility: "unlisted", ModrinthProjectID: "target-binding"}
	empty.ID, err = st.CreatePack(empty)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpdatePack(empty); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			cp := *empty
			_, _, e := st.ForkPackBuild(ctx, makeFork(&cp, fmt.Sprintf("concurrent-%d", index)))
			results <- e
		}(i)
	}
	group.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("empty target accepted %d concurrent forks", success)
	}
	builds, err := st.ListPackBuilds(empty.ID)
	if err != nil || len(builds) != 1 {
		t.Fatal("failed fork left partial builds")
	}
	after, err := st.GetPack(empty.ID)
	if err != nil || after.Summary != "Keep settings" || after.ModrinthProjectID != "target-binding" {
		t.Fatal("existing target settings were overwritten")
	}
	if _, err = db.Exec(`DELETE FROM users WHERE id=$1`, users[0].ID); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListBuildContent(forkBuild)
	if err != nil || len(rows) != 1 || !strings.HasPrefix(rows[0].StorageKey, "packs/target-owner/") {
		t.Fatal("source deletion damaged fork")
	}
}

func TestIntegrationPackWatchFollowsRecommendationAndSurvivesSourceDeletion(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	ctx := context.Background()
	user := &models.User{Username: uniqueName("follow_"), Password: "test"}
	user.Email = user.Username + "@example.test"
	if err := st.CreateUser(user); err != nil {
		t.Fatal(err)
	}
	p := &models.Pack{OwnerID: user.ID, InternalName: "Live pack", InternalSlug: "live-pack", ModrinthVisibility: "unlisted"}
	id, err := st.CreatePack(p)
	if err != nil {
		t.Fatal(err)
	}
	p.ID = id
	builds := []*models.PackBuild{{PackID: id, VersionString: "1.0", Channel: "draft"}, {PackID: id, VersionString: "1.1", Channel: "draft"}, {PackID: id, VersionString: "2.0-draft", Channel: "draft"}}
	for _, b := range builds {
		b.ID, err = st.CreatePackBuild(b)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = st.SetPackRecommended(ctx, id, builds[0].ID); err != nil {
		t.Fatal(err)
	}
	w := models.ModrinthWatch{UserID: user.ID, PackID: id, FollowPack: true, CheckedBuildID: builds[0].ID, CheckedBuildVersion: "1.0", Title: "Live pack / 1.0", Loader: "forge", Current: "1.20.1", Targets: []string{"1.21.1"}, Future: true, Projects: []models.ModrinthWatchProject{{ProjectID: "project", Title: "Mod"}}}
	if err = st.CreateModrinthWatch(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if err = st.SetPackRecommended(ctx, id, builds[1].ID); err != nil {
		t.Fatal(err)
	}
	ready := []models.ModrinthWatchVersion{{Minecraft: "1.21.1", Available: 1, Missing: []string{}}}
	if err = st.CompleteModrinthWatch(ctx, w, ready, ""); err != nil {
		t.Fatal(err)
	}
	notices, err := st.ListNotifications(user.ID, true, 100)
	if err != nil || len(notices) != 0 {
		t.Fatal("obsolete recommended source notified")
	}
	w.CheckedBuildID = builds[1].ID
	w.CheckedBuildVersion = "1.1"
	w.Title = "Live pack / 1.1"
	if err = st.CompleteModrinthWatch(ctx, w, ready, ""); err != nil {
		t.Fatal(err)
	}
	notices, err = st.ListNotifications(user.ID, true, 100)
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0].Title, "1.1") || notices[0].Link != fmt.Sprintf("/modpacks/%d/builds/%d", id, builds[1].ID) {
		t.Fatal("current source was not named in notification")
	}
	if err = st.DeletePackBuild(builds[0].ID, id); err != nil {
		t.Fatal(err)
	}
	watches, err := st.ListModrinthWatches(ctx, user.ID, false)
	if err != nil || len(watches) != 1 || watches[0].BuildID != nil || !watches[0].FollowPack || watches[0].CheckedBuildVersion != "1.1" {
		t.Fatal("old source deletion removed pack alert or lost source metadata")
	}
	if err = st.SetPackRecommended(ctx, id, builds[2].ID); err != nil {
		t.Fatal(err)
	}
	w.CheckedBuildID = builds[2].ID
	w.CheckedBuildVersion = "2.0-draft"
	w.Title = "Live pack / 2.0-draft"
	if err = st.CompleteModrinthWatch(ctx, w, ready, ""); err != nil {
		t.Fatal(err)
	}
	notices, err = st.ListNotifications(user.ID, true, 100)
	if err != nil || len(notices) != 1 {
		t.Fatal("source switch repeated an already delivered Minecraft version")
	}
	if err = st.SetPackRecommended(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetPack(id)
	if err != nil || p.RecommendedBuildID != nil {
		t.Fatal("automatic source was not restored")
	}
	all, err := st.ListPackBuilds(id)
	if err != nil || models.RecommendedPackBuild(p, all) != nil {
		t.Fatal("automatic source selected a draft")
	}
	builds[1].Channel = "release"
	if err = st.UpdatePackBuild(builds[1]); err != nil {
		t.Fatal(err)
	}
	all, err = st.ListPackBuilds(id)
	if err != nil {
		t.Fatal(err)
	}
	chosen := models.RecommendedPackBuild(p, all)
	if chosen == nil || chosen.ID != builds[1].ID {
		t.Fatal("newest release fallback not selected")
	}
	watches, err = st.ListModrinthWatches(ctx, user.ID, false)
	if err != nil || len(watches) != 1 || watches[0].NextCheck.After(time.Now().Add(time.Second)) {
		t.Fatal("source update did not wake pack check")
	}
	if err = st.DeletePack(id, user.ID); err != nil {
		t.Fatal(err)
	}
	watches, err = st.ListModrinthWatches(ctx, user.ID, false)
	if err != nil || len(watches) != 0 {
		t.Fatal("pack deletion did not clean up its alert")
	}
}
