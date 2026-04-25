package store

import (
	"context"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FirestoreStore implements Store using Cloud Firestore (metadata) and GCS (blobs).
// Per §5.3, both clients are wrapped in a single struct.
// bucket is the single unified GCS bucket for all blobs (builds, sessions, stories, source files).
type FirestoreStore struct {
	fsClient  *firestore.Client
	gcsClient *storage.Client
	bucket    *storage.BucketHandle
}

// NewFirestoreStore creates a new FirestoreStore.
// projectID is the GCP project, databaseName is the Firestore DB,
// bucketName is the single unified GCS bucket for all blob storage.
func NewFirestoreStore(ctx context.Context, projectID, databaseName, bucketName string, opts ...option.ClientOption) (*FirestoreStore, error) {
	fsClient, err := firestore.NewClientWithDatabase(ctx, projectID, databaseName, opts...)
	if err != nil {
		return nil, fmt.Errorf("firestore client: %w", err)
	}
	gcsClient, err := storage.NewClient(ctx, opts...)
	if err != nil {
		fsClient.Close() //nolint:errcheck
		return nil, fmt.Errorf("gcs client: %w", err)
	}
	return &FirestoreStore{
		fsClient:  fsClient,
		gcsClient: gcsClient,
		bucket:    gcsClient.Bucket(bucketName),
	}, nil
}

// Close releases both clients.
func (s *FirestoreStore) Close() error {
	err1 := s.fsClient.Close()
	err2 := s.gcsClient.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// ---- Runs ----

func (s *FirestoreStore) CreateRun(ctx context.Context, r *Run) error {
	doc := runToDoc(r)
	_, err := s.fsClient.Collection("runs").Doc(r.ID).Set(ctx, doc)
	return err
}

func (s *FirestoreStore) GetRun(ctx context.Context, id string) (*Run, error) {
	snap, err := s.fsClient.Collection("runs").Doc(id).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	return runFromDoc(snap)
}

func (s *FirestoreStore) UpdateRun(ctx context.Context, r *Run) error {
	doc := runToDoc(r)
	_, err := s.fsClient.Collection("runs").Doc(r.ID).Set(ctx, doc)
	return err
}

// DeleteRun removes the GCS session prefix (sessions/<id>/) then the Firestore doc.
// GCS delete is attempted first; if it fails the Firestore doc is not removed so
// the caller can retry (idempotent — a repeated DeleteBlobPrefix on a missing prefix is safe).
func (s *FirestoreStore) DeleteRun(ctx context.Context, id string) error {
	if _, err := s.DeleteBlobPrefix(ctx, "sessions/"+id+"/"); err != nil {
		return fmt.Errorf("delete session GCS prefix: %w", err)
	}
	_, err := s.fsClient.Collection("runs").Doc(id).Delete(ctx)
	return err
}

// DeleteAbandonedPendingRuns sweeps runs with status=="pending" and createdAt < before.
// Uses BulkWriter for the Firestore deletes. GCS prefix is attempted first per run;
// runs whose GCS delete fails are skipped (retried on next sweep).
func (s *FirestoreStore) DeleteAbandonedPendingRuns(ctx context.Context, before time.Time) (int, error) {
	bw := s.fsClient.BulkWriter(ctx)
	total := 0

	for {
		iter := s.fsClient.Collection("runs").
			Where("status", "==", "pending").
			Where("createdAt", "<", before).
			Limit(500).
			Documents(ctx)

		type pendingRun struct {
			ref *firestore.DocumentRef
			id  string
		}
		var runs []pendingRun
		for {
			doc, err := iter.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				iter.Stop()
				bw.End()
				return total, fmt.Errorf("listing abandoned pending runs: %w", err)
			}
			runs = append(runs, pendingRun{ref: doc.Ref, id: doc.Ref.ID})
		}
		iter.Stop()

		if len(runs) == 0 {
			break
		}

		for _, r := range runs {
			if _, err := s.DeleteBlobPrefix(ctx, "sessions/"+r.id+"/"); err != nil {
				_ = err // already logged inside DeleteBlobPrefix; retry on next sweep
				continue
			}
			if _, err := bw.Delete(r.ref); err != nil {
				bw.End()
				return total, fmt.Errorf("enqueue delete: %w", err)
			}
			total++
		}
		bw.Flush()

		if len(runs) < 500 {
			break
		}
	}

	bw.End()
	return total, nil
}

// ListRunsByUser returns runs owned by userID ordered by lastActiveAt DESC.
// limit is clamped to [1, 50].
func (s *FirestoreStore) ListRunsByUser(ctx context.Context, userID string, limit int) ([]*Run, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	iter := s.fsClient.Collection("runs").
		Where("userId", "==", userID).
		OrderBy("lastActiveAt", firestore.Desc).
		Limit(limit).
		Documents(ctx)
	defer iter.Stop()

	var runs []*Run
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list runs by user: %w", err)
		}
		r, err := runFromDoc(doc)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// ListRunsByProject returns runs with projectId == projectID.
// limit is clamped to [1, 500].
func (s *FirestoreStore) ListRunsByProject(ctx context.Context, projectID string, limit int) ([]*Run, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	iter := s.fsClient.Collection("runs").
		Where("projectId", "==", projectID).
		Limit(limit).
		Documents(ctx)
	defer iter.Stop()

	var runs []*Run
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list runs by project: %w", err)
		}
		r, err := runFromDoc(doc)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// ---- Projects ----

func (s *FirestoreStore) CreateProject(ctx context.Context, p *Project) error {
	doc := projectToDoc(p)
	_, err := s.fsClient.Collection("projects").Doc(p.ID).Set(ctx, doc)
	return err
}

func (s *FirestoreStore) GetProject(ctx context.Context, id string) (*Project, error) {
	snap, err := s.fsClient.Collection("projects").Doc(id).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	return projectFromDoc(snap)
}

func (s *FirestoreStore) UpdateProjectSource(ctx context.Context, id, source string, updatedAt time.Time) error {
	_, err := s.fsClient.Collection("projects").Doc(id).Set(ctx, map[string]interface{}{
		"source":    source,
		"updatedAt": updatedAt,
	}, firestore.MergeAll)
	return err
}

func (s *FirestoreStore) UpdateProjectMeta(ctx context.Context, id, name, description string, updatedAt time.Time) error {
	_, err := s.fsClient.Collection("projects").Doc(id).Set(ctx, map[string]interface{}{
		"name":        name,
		"description": description,
		"updatedAt":   updatedAt,
	}, firestore.MergeAll)
	return err
}

func (s *FirestoreStore) UpdateProjectLatestBuild(ctx context.Context, id, buildID string) error {
	_, err := s.fsClient.Collection("projects").Doc(id).Set(ctx, map[string]interface{}{
		"latestBuildId": buildID,
		"updatedAt":     time.Now().UTC(),
	}, firestore.MergeAll)
	return err
}

func (s *FirestoreStore) ListProjectsByOwner(ctx context.Context, ownerUID string, limit int) ([]*Project, error) {
	iter := s.fsClient.Collection("projects").
		Where("ownerUid", "==", ownerUID).
		OrderBy("updatedAt", firestore.Desc).
		Limit(limit).
		Documents(ctx)
	defer iter.Stop()

	var projects []*Project
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		p, err := projectFromDoc(doc)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

// ---- Builds ----

func (s *FirestoreStore) CreateBuild(ctx context.Context, b *Build) error {
	doc := buildToDoc(b)
	_, err := s.fsClient.Collection("builds").Doc(b.ID).Set(ctx, doc)
	return err
}

func (s *FirestoreStore) GetBuild(ctx context.Context, id string) (*Build, error) {
	snap, err := s.fsClient.Collection("builds").Doc(id).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	return buildFromDoc(snap)
}

func (s *FirestoreStore) UpdateBuild(ctx context.Context, b *Build) error {
	doc := buildToDoc(b)
	_, err := s.fsClient.Collection("builds").Doc(b.ID).Set(ctx, doc)
	return err
}

func (s *FirestoreStore) ListBuildsByProject(ctx context.Context, projectID string, limit int) ([]*Build, error) {
	iter := s.fsClient.Collection("builds").
		Where("projectId", "==", projectID).
		OrderBy("createdAt", firestore.Desc).
		Limit(limit).
		Documents(ctx)
	defer iter.Stop()

	var builds []*Build
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		b, err := buildFromDoc(doc)
		if err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, nil
}

// DeleteBuildsForProject removes all builds for the given project using BulkWriter.
func (s *FirestoreStore) DeleteBuildsForProject(ctx context.Context, projectID string) (int, error) {
	bw := s.fsClient.BulkWriter(ctx)
	total := 0

	for {
		iter := s.fsClient.Collection("builds").
			Where("projectId", "==", projectID).
			Limit(500).
			Documents(ctx)

		var refs []*firestore.DocumentRef
		for {
			doc, err := iter.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				iter.Stop()
				bw.End()
				return total, fmt.Errorf("listing builds: %w", err)
			}
			refs = append(refs, doc.Ref)
		}
		iter.Stop()

		if len(refs) == 0 {
			break
		}

		for _, ref := range refs {
			if _, err := bw.Delete(ref); err != nil {
				bw.End()
				return total, fmt.Errorf("enqueue delete: %w", err)
			}
			total++
		}
		bw.Flush()

		if len(refs) < 500 {
			break
		}
	}

	bw.End()
	return total, nil
}

// DeleteRunsForProject removes all run Firestore documents for a project using
// BulkWriter. GCS blobs must be cleaned up by the caller before this call.
func (s *FirestoreStore) DeleteRunsForProject(ctx context.Context, projectID string) (int, error) {
	bw := s.fsClient.BulkWriter(ctx)
	total := 0

	for {
		iter := s.fsClient.Collection("runs").
			Where("projectId", "==", projectID).
			Limit(500).
			Documents(ctx)

		var refs []*firestore.DocumentRef
		for {
			doc, err := iter.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				iter.Stop()
				bw.End()
				return total, fmt.Errorf("listing runs for project: %w", err)
			}
			refs = append(refs, doc.Ref)
		}
		iter.Stop()

		if len(refs) == 0 {
			break
		}

		for _, ref := range refs {
			if _, err := bw.Delete(ref); err != nil {
				bw.End()
				return total, fmt.Errorf("enqueue delete run: %w", err)
			}
			total++
		}
		bw.Flush()

		if len(refs) < 500 {
			break
		}
	}

	bw.End()
	return total, nil
}

// DeleteProject removes the Firestore project document.
// All GCS blobs and subcollections must be deleted by the caller first.
func (s *FirestoreStore) DeleteProject(ctx context.Context, id string) error {
	_, err := s.fsClient.Collection("projects").Doc(id).Delete(ctx)
	return err
}

// ---- AI / publish / community ----

// UpdateProjectAI writes updated project metadata and appends an ai_turns doc
// via BulkWriter. The project source itself must be written to GCS by the caller
// before this call. Source snapshots inside the turn are written to GCS here
// before the BulkWriter flush. If turn is nil, only the project doc is updated.
func (s *FirestoreStore) UpdateProjectAI(ctx context.Context, p *Project, turn *AITurn) (time.Time, error) {
	bw := s.fsClient.BulkWriter(ctx)

	projectDoc := map[string]interface{}{
		"description": p.Description,
		"updatedAt":   p.UpdatedAt,
		"source":      firestore.Delete, // converge migration (§4.4)
	}
	projectRef := s.fsClient.Collection("projects").Doc(p.ID)
	if _, err := bw.Set(projectRef, projectDoc, firestore.MergeAll); err != nil {
		bw.End()
		return time.Time{}, fmt.Errorf("enqueue project update: %w", err)
	}

	if turn != nil {
		if err := s.writeAITurnBlobs(ctx, turn); err != nil {
			bw.End()
			return time.Time{}, fmt.Errorf("write ai_turn blobs: %w", err)
		}
		turnDoc := aiTurnToDoc(turn)
		turnRef := s.fsClient.Collection("projects").Doc(p.ID).Collection("ai_turns").Doc(turn.ID)
		if _, err := bw.Set(turnRef, turnDoc); err != nil {
			bw.End()
			return time.Time{}, fmt.Errorf("enqueue ai_turn set: %w", err)
		}
	}

	bw.Flush()
	bw.End()
	return p.UpdatedAt, nil
}

// SetProjectPublished toggles the published flag and stamps publishedAt on false→true.
func (s *FirestoreStore) SetProjectPublished(ctx context.Context, projectID string, published bool, now time.Time) error {
	var doc map[string]interface{}
	if published {
		doc = map[string]interface{}{
			"published":   true,
			"publishedAt": now,
		}
	} else {
		doc = map[string]interface{}{
			"published": false,
		}
	}
	_, err := s.fsClient.Collection("projects").Doc(projectID).Set(ctx, doc, firestore.MergeAll)
	return err
}

// ListPublishedProjects returns published projects ordered by publishedAt DESC.
// limit is clamped to [1, 50]. Requires the projects_published composite index.
func (s *FirestoreStore) ListPublishedProjects(ctx context.Context, limit int) ([]*Project, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	iter := s.fsClient.Collection("projects").
		Where("published", "==", true).
		OrderBy("publishedAt", firestore.Desc).
		Limit(limit).
		Documents(ctx)
	defer iter.Stop()

	var projects []*Project
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list published projects: %w", err)
		}
		p, err := projectFromDoc(doc)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

// CreateAITurn writes one ai_turns document. Used to record failed turns.
// Source snapshots (SourceBefore, SourceAfter) are written to GCS before the
// Firestore document so that a Firestore write failure leaves no orphan blobs.
func (s *FirestoreStore) CreateAITurn(ctx context.Context, t *AITurn) error {
	if err := s.writeAITurnBlobs(ctx, t); err != nil {
		return fmt.Errorf("create ai_turn blobs: %w", err)
	}
	doc := aiTurnToDoc(t)
	turnRef := s.fsClient.Collection("projects").Doc(t.ProjectID).Collection("ai_turns").Doc(t.ID)
	_, err := turnRef.Set(ctx, doc)
	return err
}

// ListAIConversation returns all turns for a project ordered chronologically
// (oldest first). limit is clamped to [1, 200].
// SourceBefore and SourceAfter are fetched from GCS in parallel after the
// Firestore query; missing blobs are silently treated as empty string.
func (s *FirestoreStore) ListAIConversation(ctx context.Context, projectID string, limit int) ([]*AITurn, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	iter := s.fsClient.Collection("projects").Doc(projectID).Collection("ai_turns").
		OrderBy("id", firestore.Asc).
		Limit(limit).
		Documents(ctx)
	defer iter.Stop()

	var turns []*AITurn
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list ai conversation: %w", err)
		}
		t, err := aiTurnFromDoc(doc)
		if err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	if len(turns) > 0 {
		s.readAITurnBlobs(ctx, turns)
	}
	return turns, nil
}

// DeleteAIConversation removes every ai_turns document for a project using BulkWriter,
// then removes all GCS source blobs under the ai-turns/{projectID}/ prefix.
func (s *FirestoreStore) DeleteAIConversation(ctx context.Context, projectID string) (int, error) {
	bw := s.fsClient.BulkWriter(ctx)
	total := 0

	for {
		iter := s.fsClient.Collection("projects").Doc(projectID).Collection("ai_turns").
			Limit(500).
			Documents(ctx)

		var refs []*firestore.DocumentRef
		for {
			doc, err := iter.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				iter.Stop()
				bw.End()
				return total, fmt.Errorf("listing ai_turns: %w", err)
			}
			refs = append(refs, doc.Ref)
		}
		iter.Stop()

		if len(refs) == 0 {
			break
		}

		for _, ref := range refs {
			if _, err := bw.Delete(ref); err != nil {
				bw.End()
				return total, fmt.Errorf("enqueue delete ai_turn: %w", err)
			}
			total++
		}
		bw.Flush()

		if len(refs) < 500 {
			break
		}
	}

	bw.End()

	// Remove all GCS source blobs for this project's AI turns.
	if _, err := s.DeleteBlobPrefix(ctx, "ai-turns/"+projectID+"/"); err != nil {
		return total, fmt.Errorf("delete ai_turn blobs: %w", err)
	}
	return total, nil
}

// ---- IFDB cache ----

func (s *FirestoreStore) GetCachedGame(ctx context.Context, tuid string) (*CachedGame, error) {
	snap, err := s.fsClient.Collection("ifdb_cache").Doc(tuid).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}

	data := snap.Data()
	expiresAt, _ := data["expiresAt"].(time.Time)
	if !expiresAt.After(time.Now()) {
		// Stale — delete asynchronously and return nil.
		go func() {
			bw := s.fsClient.BulkWriter(context.Background())
			_, _ = bw.Delete(snap.Ref)
			bw.End()
		}()
		return nil, nil
	}

	payload, _ := data["payload"].(string)
	fetchedAt, _ := data["fetchedAt"].(time.Time)
	return &CachedGame{
		TUID:      tuid,
		Payload:   []byte(payload),
		FetchedAt: fetchedAt,
		ExpiresAt: expiresAt,
	}, nil
}

func (s *FirestoreStore) PutCachedGame(ctx context.Context, g *CachedGame) error {
	doc := map[string]interface{}{
		"tuid":      g.TUID,
		"payload":   string(g.Payload),
		"fetchedAt": g.FetchedAt,
		"expiresAt": g.ExpiresAt,
	}
	_, err := s.fsClient.Collection("ifdb_cache").Doc(g.TUID).Set(ctx, doc)
	return err
}

func (s *FirestoreStore) ListFreshCachedGames(ctx context.Context, now time.Time) ([]*CachedGame, error) {
	iter := s.fsClient.Collection("ifdb_cache").
		Where("expiresAt", ">", now).
		Documents(ctx)
	defer iter.Stop()

	var games []*CachedGame
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		data := doc.Data()
		tuid, _ := data["tuid"].(string)
		payload, _ := data["payload"].(string)
		fetchedAt, _ := data["fetchedAt"].(time.Time)
		expiresAt, _ := data["expiresAt"].(time.Time)
		games = append(games, &CachedGame{
			TUID:      tuid,
			Payload:   []byte(payload),
			FetchedAt: fetchedAt,
			ExpiresAt: expiresAt,
		})
	}
	return games, nil
}

// ---- helpers: document ↔ struct conversion ----

func runToDoc(r *Run) map[string]interface{} {
	doc := map[string]interface{}{
		"id":             r.ID,
		"sourceType":     r.SourceType,
		"ifdbId":         r.IFDBId,
		"title":          r.Title,
		"format":         r.Format,
		"artifactUrl":    r.ArtifactURL,
		"buildId":        r.BuildID,
		"userId":         r.UserID,
		"status":         r.Status,
		"createdAt":      r.CreatedAt,
		"transcriptPath": r.TranscriptPath,
		"errorCode":      r.ErrorCode,
		"errorMessage":   r.ErrorMessage,
		"interpreter":    r.Interpreter,
		"storyPath":      r.StoryPath,
		"savePath":       r.SavePath,
		"turnCount":      r.TurnCount,
		"reconnectCount": r.ReconnectCount,
		"candidateUrls":  r.CandidateURLs,
	}
	if r.StartedAt != nil {
		doc["startedAt"] = *r.StartedAt
	} else {
		doc["startedAt"] = nil
	}
	if r.FinishedAt != nil {
		doc["finishedAt"] = *r.FinishedAt
	} else {
		doc["finishedAt"] = nil
	}
	if r.ExitCode != nil {
		doc["exitCode"] = *r.ExitCode
	} else {
		doc["exitCode"] = nil
	}
	if r.LastActiveAt != nil {
		doc["lastActiveAt"] = *r.LastActiveAt
	} else {
		doc["lastActiveAt"] = nil
	}
	if r.LastSaveAt != nil {
		doc["lastSaveAt"] = *r.LastSaveAt
	} else {
		doc["lastSaveAt"] = nil
	}
	return doc
}

func runFromDoc(snap *firestore.DocumentSnapshot) (*Run, error) {
	data := snap.Data()
	r := &Run{
		ID:             stringField(data, "id"),
		SourceType:     stringField(data, "sourceType"),
		IFDBId:         stringField(data, "ifdbId"),
		Title:          stringField(data, "title"),
		Format:         stringField(data, "format"),
		ArtifactURL:    stringField(data, "artifactUrl"),
		BuildID:        stringField(data, "buildId"),
		ProjectID:      stringField(data, "projectId"),
		UserID:         stringField(data, "userId"),
		Status:         stringField(data, "status"),
		TranscriptPath: stringField(data, "transcriptPath"),
		ErrorCode:      stringField(data, "errorCode"),
		ErrorMessage:   stringField(data, "errorMessage"),
		Interpreter:    stringField(data, "interpreter"),
		StoryPath:      stringField(data, "storyPath"),
		SavePath:       stringField(data, "savePath"),
	}
	if v, ok := data["turnCount"].(int64); ok {
		r.TurnCount = int(v)
	}
	if v, ok := data["reconnectCount"].(int64); ok {
		r.ReconnectCount = int(v)
	}
	if raw, ok := data["candidateUrls"].([]interface{}); ok {
		urls := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok {
				urls = append(urls, s)
			}
		}
		r.CandidateURLs = urls
	}
	if v, ok := data["createdAt"].(time.Time); ok {
		r.CreatedAt = v
	}
	if v, ok := data["startedAt"].(time.Time); ok {
		r.StartedAt = &v
	}
	if v, ok := data["finishedAt"].(time.Time); ok {
		r.FinishedAt = &v
	}
	if v, ok := data["exitCode"].(int64); ok {
		ec := int(v)
		r.ExitCode = &ec
	}
	if v, ok := data["lastActiveAt"].(time.Time); ok {
		r.LastActiveAt = &v
	}
	if v, ok := data["lastSaveAt"].(time.Time); ok {
		r.LastSaveAt = &v
	}
	return r, nil
}

func projectToDoc(p *Project) map[string]interface{} {
	// Note: "source" is intentionally omitted — source text lives in GCS.
	// New projects never write source to Firestore; legacy migration is handled
	// by GetProjectSource (§4.4 of ARCHITECTURE_AI_CREATE.md).
	doc := map[string]interface{}{
		"id":            p.ID,
		"ownerUid":      p.OwnerUID,
		"name":          p.Name,
		"description":   p.Description,
		"published":     p.Published,
		"createdAt":     p.CreatedAt,
		"updatedAt":     p.UpdatedAt,
		"latestBuildId": p.LatestBuildID,
	}
	if p.PublishedAt != nil {
		doc["publishedAt"] = *p.PublishedAt
	}
	return doc
}

func projectFromDoc(snap *firestore.DocumentSnapshot) (*Project, error) {
	data := snap.Data()
	p := &Project{
		ID:            stringField(data, "id"),
		OwnerUID:      stringField(data, "ownerUid"),
		Name:          stringField(data, "name"),
		Source:        stringField(data, "source"), // legacy migration field; see GetProjectSource (§4.4)
		Description:   stringField(data, "description"),
		LatestBuildID: stringField(data, "latestBuildId"),
	}
	if v, ok := data["createdAt"].(time.Time); ok {
		p.CreatedAt = v
	}
	if v, ok := data["updatedAt"].(time.Time); ok {
		p.UpdatedAt = v
	}
	if v, ok := data["published"].(bool); ok {
		p.Published = v
	}
	if v, ok := data["publishedAt"].(time.Time); ok {
		p.PublishedAt = &v
	}
	return p, nil
}

func buildToDoc(b *Build) map[string]interface{} {
	doc := map[string]interface{}{
		"id":             b.ID,
		"projectId":      b.ProjectID,
		"ownerUid":       b.OwnerUID,
		"status":         b.Status,
		"createdAt":      b.CreatedAt,
		"artifactFormat": b.ArtifactFormat,
		"artifactPath":   b.ArtifactPath,
		"logPath":        b.LogPath,
		"errorMessage":   b.ErrorMessage,
	}
	if b.StartedAt != nil {
		doc["startedAt"] = *b.StartedAt
	} else {
		doc["startedAt"] = nil
	}
	if b.FinishedAt != nil {
		doc["finishedAt"] = *b.FinishedAt
	} else {
		doc["finishedAt"] = nil
	}
	return doc
}

func buildFromDoc(snap *firestore.DocumentSnapshot) (*Build, error) {
	data := snap.Data()
	b := &Build{
		ID:             stringField(data, "id"),
		ProjectID:      stringField(data, "projectId"),
		OwnerUID:       stringField(data, "ownerUid"),
		Status:         stringField(data, "status"),
		ArtifactFormat: stringField(data, "artifactFormat"),
		ArtifactPath:   stringField(data, "artifactPath"),
		LogPath:        stringField(data, "logPath"),
		ErrorMessage:   stringField(data, "errorMessage"),
	}
	if v, ok := data["createdAt"].(time.Time); ok {
		b.CreatedAt = v
	}
	if v, ok := data["startedAt"].(time.Time); ok {
		b.StartedAt = &v
	}
	if v, ok := data["finishedAt"].(time.Time); ok {
		b.FinishedAt = &v
	}
	return b, nil
}

func stringField(data map[string]interface{}, key string) string {
	v, _ := data[key].(string)
	return v
}

func aiTurnToDoc(t *AITurn) map[string]interface{} {
	// SourceBefore and SourceAfter are stored in GCS, not in the Firestore document.
	return map[string]interface{}{
		"id":               t.ID,
		"projectId":        t.ProjectID,
		"ownerUid":         t.OwnerUID,
		"kind":             t.Kind,
		"userMessage":      t.UserMessage,
		"assistantReply":   t.AssistantReply,
		"modelRequestedAt": t.ModelRequestedAt,
		"modelFinishedAt":  t.ModelFinishedAt,
		"promptTokens":     t.PromptTokens,
		"completionTokens": t.CompletionTokens,
		"model":            t.Model,
		"error":            t.Error,
	}
}

func aiTurnFromDoc(snap *firestore.DocumentSnapshot) (*AITurn, error) {
	data := snap.Data()
	// SourceBefore and SourceAfter are not stored in the Firestore document;
	// they are fetched from GCS by readAITurnBlobs after the list query.
	t := &AITurn{
		ID:             stringField(data, "id"),
		ProjectID:      stringField(data, "projectId"),
		OwnerUID:       stringField(data, "ownerUid"),
		Kind:           stringField(data, "kind"),
		UserMessage:    stringField(data, "userMessage"),
		AssistantReply: stringField(data, "assistantReply"),
		Model:          stringField(data, "model"),
		Error:          stringField(data, "error"),
	}
	if v, ok := data["modelRequestedAt"].(time.Time); ok {
		t.ModelRequestedAt = v
	}
	if v, ok := data["modelFinishedAt"].(time.Time); ok {
		t.ModelFinishedAt = v
	}
	if v, ok := data["promptTokens"].(int64); ok {
		t.PromptTokens = int(v)
	}
	if v, ok := data["completionTokens"].(int64); ok {
		t.CompletionTokens = int(v)
	}
	return t, nil
}

// UploadBlob, DownloadBlob, SignedReadURL are in gcs.go.
var _ io.Reader // keep io import used
