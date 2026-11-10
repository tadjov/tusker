package datapackage_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/s3mock"

	"github.com/tadjov/tusker/internal/datapackage"
)

const s3MockImage = "adobe/s3mock:5.2.3@sha256:ab01a6946750f451ca215a47e91030695b260e4003b8a5a6201d25029b8fca92"

// servePackage serves root over TLS with the package in root/pkg, not at the server root.
func servePackage(t *testing.T) (root string, written datapackage.Package, srv *httptest.Server) {
	t.Helper()
	root = t.TempDir()
	written = writeFullPackage(t, filepath.Join(root, "pkg"))
	srv = httptest.NewTLSServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)
	return root, written, srv
}

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s still holds %v", dir, entries)
	}
}

func TestOpenOverHTTPSServesVerifiedResources(t *testing.T) {
	root, written, srv := servePackage(t)
	tempDir := t.TempDir()
	opts := datapackage.Options{HTTPClient: srv.Client(), TempDir: tempDir}
	ctx := context.Background()

	opened, err := datapackage.OpenWith(ctx, srv.URL+"/pkg/datapackage.json", opts)
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	if !reflect.DeepEqual(opened.Package, written) {
		t.Errorf("descriptor differs:\n got %+v\nwant %+v", opened.Package, written)
	}
	got, err := readResource(ctx, opened, "articles")
	if err != nil || string(got) != "not really parquet" {
		t.Errorf("articles = %q, %v", got, err)
	}
	emptyDir(t, tempDir)

	writeFile(t, filepath.Join(root, "pkg"), "articles.parquet", strings.Replace("not really parquet", "p", "P", 1))
	_, err = readResource(ctx, opened, "articles")
	var mismatch *datapackage.HashMismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("flipped byte: err = %v, want HashMismatchError", err)
	}
	emptyDir(t, tempDir)

	if err := os.Remove(filepath.Join(root, "pkg", "embeddings.parquet")); err != nil {
		t.Fatal(err)
	}
	_, err = readResource(ctx, opened, "embeddings")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("404: err = %v, want NotFoundError", err)
	}
}

func TestOpenRefusesPlainHTTP(t *testing.T) {
	_, _, srv := servePackage(t)
	plain := httptest.NewServer(srv.Config.Handler)
	t.Cleanup(plain.Close)

	if _, err := datapackage.Open(context.Background(), plain.URL+"/pkg/datapackage.json"); err == nil {
		t.Error("Open accepted an http:// descriptor")
	}
}

// S3Mock accepts any credentials, so these tests don't cover signing.
func startS3Mock(t *testing.T) *s3.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Docker")
	}
	ctx := context.Background()
	container, err := s3mock.Run(ctx, s3MockImage)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("start s3mock: %v", err)
	}
	endpoint, err := container.EndpointURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const accessKey, secretKey = "fixture", "fixture"
	t.Setenv("DATASET_S3_ENDPOINT", endpoint)
	t.Setenv("DATASET_S3_REGION", "auto")
	t.Setenv("DATASET_S3_ACCESS_KEY", accessKey)
	t.Setenv("DATASET_S3_SECRET_KEY", secretKey)
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "auto",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
	})
}

func upload(t *testing.T, client *s3.Client, bucket, localDir, prefix string) {
	t.Helper()
	err := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		body, err := os.Open(path)
		if err != nil {
			return err
		}
		defer body.Close()
		_, err = client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(prefix + filepath.ToSlash(rel)),
			Body:   body,
		})
		return err
	})
	if err != nil {
		t.Fatalf("upload %s: %v", localDir, err)
	}
}

func TestPackageOpensIdenticallyFromDiskHTTPSAndS3(t *testing.T) {
	client := startS3Mock(t)
	root, written, srv := servePackage(t)
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("datasets")}); err != nil {
		t.Fatal(err)
	}
	upload(t, client, "datasets", filepath.Join(root, "pkg"), "cc-news/canonical/v1/")

	locations := []struct {
		name     string
		location string
		opts     datapackage.Options
	}{
		{"disk", filepath.Join(root, "pkg"), datapackage.Options{}},
		{"https", srv.URL + "/pkg/datapackage.json", datapackage.Options{HTTPClient: srv.Client()}},
		{"s3", "s3://datasets/cc-news/canonical/v1/datapackage.json", datapackage.Options{S3: datapackage.S3ConfigFromEnv()}},
		{"https folder", srv.URL + "/pkg", datapackage.Options{HTTPClient: srv.Client()}},
		{"s3 folder", "s3://datasets/cc-news/canonical/v1", datapackage.Options{S3: datapackage.S3ConfigFromEnv()}},
	}
	for _, l := range locations {
		t.Run(l.name, func(t *testing.T) {
			opened, err := datapackage.OpenWith(ctx, l.location, l.opts)
			if err != nil {
				t.Fatalf("OpenWith(%s): %v", l.location, err)
			}
			if !reflect.DeepEqual(opened.Package, written) {
				t.Errorf("descriptor differs:\n got %+v\nwant %+v", opened.Package, written)
			}
			for name, want := range map[string]string{"articles": "not really parquet", "embeddings": "not really vectors"} {
				if got, err := readResource(ctx, opened, name); err != nil || string(got) != want {
					t.Errorf("%s = %q, %v, want %q", name, got, err, want)
				}
			}
		})
	}
}

func TestOpenFromS3FailsOnAlteredOrMissingObjects(t *testing.T) {
	client := startS3Mock(t)
	root, _, _ := servePackage(t)
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("datasets")}); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "pkg")
	upload(t, client, "datasets", pkgDir, "p/")

	opened, err := datapackage.Open(ctx, "s3://datasets/p/datapackage.json")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	writeFile(t, pkgDir, "embeddings.parquet", strings.Replace("not really vectors", "v", "V", 1))
	upload(t, client, "datasets", pkgDir, "p/")
	_, err = readResource(ctx, opened, "embeddings")
	var mismatch *datapackage.HashMismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("altered object: err = %v, want HashMismatchError", err)
	}

	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("datasets"), Key: aws.String("p/articles.parquet")}); err != nil {
		t.Fatal(err)
	}
	_, err = readResource(ctx, opened, "articles")
	var missing *datapackage.NotFoundError
	if !errors.As(err, &missing) {
		t.Errorf("missing object: err = %v, want NotFoundError", err)
	}

	_, err = datapackage.Open(ctx, "s3://datasets/absent/datapackage.json")
	if !errors.As(err, &missing) {
		t.Errorf("missing descriptor: err = %v, want NotFoundError", err)
	}
}

func TestResourceStopsOnCancelWithoutLeavingADownload(t *testing.T) {
	_, _, srv := servePackage(t)
	tempDir := t.TempDir()
	opened, err := datapackage.OpenWith(context.Background(), srv.URL+"/pkg/", datapackage.Options{HTTPClient: srv.Client(), TempDir: tempDir})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = opened.Resource(ctx, "articles")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	emptyDir(t, tempDir)
}

func TestOpenOverHTTPSServesANestedResource(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/docs/notes.txt", "notes\n")
	if _, err := datapackage.Write(context.Background(), filepath.Join(root, "pkg"), datapackage.Package{
		Name:      "nested",
		Resources: []datapackage.Resource{{Name: "docs/notes.txt", Path: "docs/notes.txt", Format: "txt"}},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	srv := httptest.NewTLSServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	opened, err := datapackage.OpenWith(ctx, srv.URL+"/pkg/datapackage.json", datapackage.Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	if got, err := readResource(ctx, opened, "docs/notes.txt"); err != nil || string(got) != "notes\n" {
		t.Errorf("docs/notes.txt = %q, %v", got, err)
	}
}

// recordingS3 records each request's Authorization header.
func recordingS3(t *testing.T, root string) (endpoint string, authorizations func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	files := http.FileServer(http.Dir(root))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// ambientAWS sets AWS credentials and a missing profile that must never be read.
func ambientAWS(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AWSENVKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "AWSENVSECRET")
	t.Setenv("AWS_PROFILE", "profile-that-does-not-exist")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
}

func TestOpenFromS3SignsWithTheDatasetKeysAlone(t *testing.T) {
	root := t.TempDir()
	writeFullPackage(t, filepath.Join(root, "datasets", "pkg"))
	endpoint, authorizations := recordingS3(t, root)
	ambientAWS(t)
	t.Setenv("DATASET_S3_ENDPOINT", endpoint)
	t.Setenv("DATASET_S3_REGION", "auto")
	t.Setenv("DATASET_S3_ACCESS_KEY", "DATASETKEY")
	t.Setenv("DATASET_S3_SECRET_KEY", "DATASETSECRET")

	if _, err := datapackage.Open(context.Background(), "s3://datasets/pkg"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(authorizations()) == 0 {
		t.Fatal("no request reached the endpoint")
	}
	for _, authorization := range authorizations() {
		if !strings.Contains(authorization, "Credential=DATASETKEY/") {
			t.Errorf("Authorization = %q, want it signed with DATASETKEY", authorization)
		}
	}
}

func TestOpenFromS3WithoutDatasetKeysSendsNoAmbientCredentials(t *testing.T) {
	root := t.TempDir()
	writeFullPackage(t, filepath.Join(root, "datasets", "pkg"))
	endpoint, authorizations := recordingS3(t, root)
	ambientAWS(t)
	t.Setenv("DATASET_S3_ENDPOINT", endpoint)
	t.Setenv("DATASET_S3_ACCESS_KEY", "")
	t.Setenv("DATASET_S3_SECRET_KEY", "")

	if _, err := datapackage.Open(context.Background(), "s3://datasets/pkg"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, authorization := range authorizations() {
		if authorization != "" {
			t.Errorf("Authorization = %q, want an unsigned request", authorization)
		}
	}
}

func TestOpenFromS3RefusesAHalfConfiguredKeyPair(t *testing.T) {
	tests := []struct {
		name, accessKey, secretKey, missing string
	}{
		{"secret missing", "DATASETKEY", "", "DATASET_S3_SECRET_KEY"},
		{"access key missing", "", "DATASETSECRET", "DATASET_S3_ACCESS_KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint, authorizations := recordingS3(t, t.TempDir())
			ambientAWS(t)
			t.Setenv("DATASET_S3_ENDPOINT", endpoint)
			t.Setenv("DATASET_S3_ACCESS_KEY", tt.accessKey)
			t.Setenv("DATASET_S3_SECRET_KEY", tt.secretKey)

			_, err := datapackage.Open(context.Background(), "s3://datasets/pkg")
			if err == nil || !strings.Contains(err.Error(), tt.missing) {
				t.Errorf("err = %v, want one naming %s", err, tt.missing)
			}
			if len(authorizations()) != 0 {
				t.Errorf("%d requests were sent before the error", len(authorizations()))
			}
		})
	}
}
