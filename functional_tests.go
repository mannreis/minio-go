//go:build mint
// +build mint

/*
 * MinIO Go Library for Amazon S3 Compatible Cloud Storage
 * Copyright 2015-2020 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"iter"
	"log/slog"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/uuid"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"
	"github.com/minio/minio-go/v7/pkg/notification"
	"github.com/minio/minio-go/v7/pkg/tags"
)

const letterBytes = "abcdefghijklmnopqrstuvwxyz01234569"
const (
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

const (
	serverEndpoint     = "SERVER_ENDPOINT"
	accessKey          = "ACCESS_KEY"
	secretKey          = "SECRET_KEY"
	enableHTTPS        = "ENABLE_HTTPS"
	enableKMS          = "ENABLE_KMS"
	appVersion         = "0.1.0"
	skipCERTValidation = "SKIP_CERT_VALIDATION"
)

func createHTTPTransport() (transport *http.Transport) {
	var err error
	transport, err = minio.DefaultTransport(mustParseBool(os.Getenv(enableHTTPS)))
	if err != nil {
		logError("http-transport", getFuncName(), nil, time.Now(), "", "could not create http transport", err)
		return nil
	}

	if mustParseBool(os.Getenv(enableHTTPS)) && mustParseBool(os.Getenv(skipCERTValidation)) {
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	return
}

var readFull = func(r io.Reader, buf []byte) (n int, err error) {
	// ReadFull reads exactly len(buf) bytes from r into buf.
	// It returns the number of bytes copied and an error if
	// fewer bytes were read. The error is EOF only if no bytes
	// were read. If an EOF happens after reading some but not
	// all the bytes, ReadFull returns ErrUnexpectedEOF.
	// On return, n == len(buf) if and only if err == nil.
	// If r returns an error having read at least len(buf) bytes,
	// the error is dropped.
	for n < len(buf) && err == nil {
		var nn int
		nn, err = r.Read(buf[n:])
		// Some spurious io.Reader's return
		// io.ErrUnexpectedEOF when nn == 0
		// this behavior is undocumented
		// so we are on purpose not using io.ReadFull
		// implementation because this can lead
		// to custom handling, to avoid that
		// we simply modify the original io.ReadFull
		// implementation to avoid this issue.
		// io.ErrUnexpectedEOF with nn == 0 really
		// means that io.EOF
		if err == io.ErrUnexpectedEOF && nn == 0 {
			err = io.EOF
		}
		n += nn
	}
	if n >= len(buf) {
		err = nil
	} else if n > 0 && err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return
}

func baseLogger(testName, function string, args map[string]interface{}, startTime time.Time) *slog.Logger {
	// calculate the test case duration
	duration := time.Since(startTime)
	// log with the fields as per mint
	l := slog.With(
		"name", "minio-go: "+testName,
		"duration", duration.Nanoseconds()/1000000,
	)
	if function != "" {
		l = l.With("function", function)
	}
	if len(args) > 0 {
		l = l.With("args", args)
	}
	return l
}

// log successful test runs
func logSuccess(testName, function string, args map[string]interface{}, startTime time.Time) {
	baseLogger(testName, function, args, startTime).
		With("status", "PASS").
		Info("")
}

// As few of the features are not available in Gateway(s) currently, Check if err value is NotImplemented,
// and log as NA in that case and continue execution. Otherwise log as failure and return
func logError(testName, function string, args map[string]interface{}, startTime time.Time, alert, message string, err error) {
	// If server returns NotImplemented we assume it is gateway mode and hence log it as info and move on to next tests
	// Special case for ComposeObject API as it is implemented on client side and adds specific error details like `Error in upload-part-copy` in
	// addition to NotImplemented error returned from server
	if isErrNotImplemented(err) {
		logIgnored(testName, function, args, startTime, message)
	} else {
		logFailure(testName, function, args, startTime, alert, message, err)
		if !isRunOnFail() {
			panic(fmt.Sprintf("Test failed with message: %s, err: %v", message, err))
		}
	}
}

// Log failed test runs, do not call this directly, use logError instead, as that correctly stops the test run
func logFailure(testName, function string, args map[string]interface{}, startTime time.Time, alert, message string, err error) {
	l := baseLogger(testName, function, args, startTime).With(
		"status", "FAIL",
		"alert", alert,
		"message", message,
	)

	if err != nil {
		l = l.With("error", err)
	}

	l.Error("")
}

// log not applicable test runs
func logIgnored(testName, function string, args map[string]interface{}, startTime time.Time, alert string) {
	baseLogger(testName, function, args, startTime).
		With(
			"status", "NA",
			"alert", strings.Split(alert, " ")[0]+" is NotImplemented",
		).Info("")
}

// Delete objects in given bucket, recursively
func cleanupBucket(bucketName string, c *minio.Client) error {
	// Create a done channel to control 'ListObjectsV2' go routine.
	doneCh := make(chan struct{})
	// Exit cleanly upon return.
	defer close(doneCh)
	// Iterate over all objects in the bucket via listObjectsV2 and delete
	for objCh := range c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{Recursive: true}) {
		if objCh.Err != nil {
			return objCh.Err
		}
		if objCh.Key != "" {
			err := c.RemoveObject(context.Background(), bucketName, objCh.Key, minio.RemoveObjectOptions{})
			if err != nil {
				return err
			}
		}
	}
	for objPartInfo := range c.ListIncompleteUploads(context.Background(), bucketName, "", true) {
		if objPartInfo.Err != nil {
			return objPartInfo.Err
		}
		if objPartInfo.Key != "" {
			err := c.RemoveIncompleteUpload(context.Background(), bucketName, objPartInfo.Key)
			if err != nil {
				return err
			}
		}
	}
	// objects are already deleted, clear the buckets now
	return c.RemoveBucket(context.Background(), bucketName)
}

func cleanupVersionedBucket(bucketName string, c *minio.Client) error {
	doneCh := make(chan struct{})
	defer close(doneCh)
	for obj := range c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if obj.Key != "" {
			err := c.RemoveObject(context.Background(), bucketName, obj.Key,
				minio.RemoveObjectOptions{VersionID: obj.VersionID, GovernanceBypass: true})
			if err != nil {
				return err
			}
		}
	}
	for objPartInfo := range c.ListIncompleteUploads(context.Background(), bucketName, "", true) {
		if objPartInfo.Err != nil {
			return objPartInfo.Err
		}
		if objPartInfo.Key != "" {
			err := c.RemoveIncompleteUpload(context.Background(), bucketName, objPartInfo.Key)
			if err != nil {
				return err
			}
		}
	}
	// objects are already deleted, clear the buckets now
	err := c.RemoveBucket(context.Background(), bucketName)
	if err != nil {
		for obj := range c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true}) {
			slog.Info("found object", "key", obj.Key, "version", obj.VersionID)
		}
	}
	return err
}

func isErrNotImplemented(err error) bool {
	return minio.ToErrorResponse(err).Code == minio.NotImplemented
}

func isRunOnFail() bool {
	return os.Getenv("RUN_ON_FAIL") == "1"
}

func init() {
	// If server endpoint is not set, all tests default to
	// using https://play.min.io
	if os.Getenv(serverEndpoint) == "" {
		os.Setenv(serverEndpoint, "play.min.io")
		os.Setenv(accessKey, "Q3AM3UQ867SPQQA43P2F")
		os.Setenv(secretKey, "zuf+tfteSlswRu7BJ86wekitnifILbZam1KYY3TG")
		os.Setenv(enableHTTPS, "1")
	}
}

var mintDataDir = os.Getenv("MINT_DATA_DIR")

func getMintDataDirFilePath(filename string) (fp string) {
	if mintDataDir == "" {
		return
	}
	return filepath.Join(mintDataDir, filename)
}

func newRandomReader(seed, size int64) io.Reader {
	return io.LimitReader(rand.New(rand.NewSource(seed)), size)
}

func mustCrcReader(r io.Reader) uint32 {
	crc := crc32.NewIEEE()
	_, err := io.Copy(crc, r)
	if err != nil {
		panic(err)
	}
	return crc.Sum32()
}

func crcMatches(r io.Reader, want uint32) error {
	crc := crc32.NewIEEE()
	_, err := io.Copy(crc, r)
	if err != nil {
		panic(err)
	}
	got := crc.Sum32()
	if got != want {
		return fmt.Errorf("crc mismatch, want %x, got %x", want, got)
	}
	return nil
}

func crcMatchesName(r io.Reader, name string) error {
	want := dataFileCRC32[name]
	crc := crc32.NewIEEE()
	_, err := io.Copy(crc, r)
	if err != nil {
		panic(err)
	}
	got := crc.Sum32()
	if got != want {
		return fmt.Errorf("crc mismatch, want %x, got %x", want, got)
	}
	return nil
}

// read data from file if it exists or optionally create a buffer of particular size
func getDataReader(fileName string) io.ReadCloser {
	if mintDataDir == "" {
		size := int64(dataFileMap[fileName])
		if _, ok := dataFileCRC32[fileName]; !ok {
			dataFileCRC32[fileName] = mustCrcReader(newRandomReader(size, size))
		}
		return io.NopCloser(newRandomReader(size, size))
	}
	reader, _ := os.Open(getMintDataDirFilePath(fileName))
	if _, ok := dataFileCRC32[fileName]; !ok {
		dataFileCRC32[fileName] = mustCrcReader(reader)
		reader.Close()
		reader, _ = os.Open(getMintDataDirFilePath(fileName))
	}
	return reader
}

// randString generates random names and prepends them with a known prefix.
func randString(n int, src rand.Source, prefix string) string {
	b := make([]byte, n)
	// A rand.Int63() generates 63 random bits, enough for letterIdxMax letters!
	for i, cache, remain := n-1, src.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
	return prefix + string(b[0:30-len(prefix)])
}

var dataFileMap = map[string]int{
	"datafile-0-b":     0,
	"datafile-1-b":     1,
	"datafile-1-kB":    1 * humanize.KiByte,
	"datafile-10-kB":   10 * humanize.KiByte,
	"datafile-33-kB":   33 * humanize.KiByte,
	"datafile-100-kB":  100 * humanize.KiByte,
	"datafile-1.03-MB": 1056 * humanize.KiByte,
	"datafile-1-MB":    1 * humanize.MiByte,
	"datafile-5-MB":    5 * humanize.MiByte,
	"datafile-6-MB":    6 * humanize.MiByte,
	"datafile-11-MB":   11 * humanize.MiByte,
	"datafile-65-MB":   65 * humanize.MiByte,
	"datafile-129-MB":  129 * humanize.MiByte,
}

var dataFileCRC32 = map[string]uint32{}

func isFullMode() bool {
	return os.Getenv("MINT_MODE") == "full"
}

func getFuncName() string {
	return getFuncNameLoc(2)
}

func getFuncNameLoc(caller int) string {
	pc, _, _, _ := runtime.Caller(caller)
	return strings.TrimPrefix(runtime.FuncForPC(pc).Name(), "main.")
}

type ClientConfig struct {
	// MinIO client configuration
	TraceOn         bool // Turn on tracing of HTTP requests and responses to stderr
	CredsV2         bool // Use V2 credentials if true, otherwise use v4
	TrailingHeaders bool // Send trailing headers in requests
}

func NewClient(config ClientConfig) (*minio.Client, error) {
	// Instantiate new MinIO client
	var creds *credentials.Credentials
	if config.CredsV2 {
		creds = credentials.NewStaticV2(os.Getenv(accessKey), os.Getenv(secretKey), "")
	} else {
		creds = credentials.NewStaticV4(os.Getenv(accessKey), os.Getenv(secretKey), "")
	}
	opts := &minio.Options{
		Creds:           creds,
		Transport:       createHTTPTransport(),
		Secure:          mustParseBool(os.Getenv(enableHTTPS)),
		TrailingHeaders: config.TrailingHeaders,
	}
	client, err := minio.New(os.Getenv(serverEndpoint), opts)
	if err != nil {
		return nil, err
	}

	if config.TraceOn {
		client.TraceOn(os.Stderr)
	}

	// Set user agent.
	client.SetAppInfo("MinIO-go-FunctionalTest", appVersion)

	return client, nil
}

// Tests bucket re-create errors.
func testMakeBucketError() {
	region := "eu-central-1"

	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "MakeBucket(bucketName, region)"
	// initialize logging params
	args := map[string]interface{}{
		"bucketName": "",
		"region":     region,
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket in 'eu-central-1'.
	if err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: region}); err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket Failed", err)
		return
	}
	defer cleanupBucket(bucketName, c)

	if err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: region}); err == nil {
		logError(testName, function, args, startTime, "", "Bucket already exists", err)
		return
	}
	// Verify valid error response from server.
	if minio.ToErrorResponse(err).Code != minio.BucketAlreadyExists &&
		minio.ToErrorResponse(err).Code != minio.BucketAlreadyOwnedByYou {
		logError(testName, function, args, startTime, "", "Invalid error returned by server", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testMetadataSizeLimit() {
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader, objectSize, opts)"
	args := map[string]interface{}{
		"bucketName":        "",
		"objectName":        "",
		"opts.UserMetadata": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client creation failed", err)
		return
	}

	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	const HeaderSizeLimit = 8 * 1024
	const UserMetadataLimit = 2 * 1024

	// Meta-data greater than the 2 KB limit of AWS - PUT calls with this meta-data should fail
	metadata := make(map[string]string)
	metadata["X-Amz-Meta-Mint-Test"] = string(bytes.Repeat([]byte("m"), 1+UserMetadataLimit-len("X-Amz-Meta-Mint-Test")))
	args["metadata"] = fmt.Sprint(metadata)

	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(nil), 0, minio.PutObjectOptions{UserMetadata: metadata})
	if err == nil {
		logError(testName, function, args, startTime, "", "Created object with user-defined metadata exceeding metadata size limits", nil)
		return
	}

	// Meta-data (headers) greater than the 8 KB limit of AWS - PUT calls with this meta-data should fail
	metadata = make(map[string]string)
	metadata["X-Amz-Mint-Test"] = string(bytes.Repeat([]byte("m"), 1+HeaderSizeLimit-len("X-Amz-Mint-Test")))
	args["metadata"] = fmt.Sprint(metadata)
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(nil), 0, minio.PutObjectOptions{UserMetadata: metadata})
	if err == nil {
		logError(testName, function, args, startTime, "", "Created object with headers exceeding header size limits", nil)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests various bucket supported formats.
func testMakeBucketRegions() {
	region := "eu-central-1"
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "MakeBucket(bucketName, region)"
	// initialize logging params
	args := map[string]interface{}{
		"bucketName": "",
		"region":     region,
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket in 'eu-central-1'.
	if err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: region}); err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	// Delete all objects and buckets
	if err = cleanupBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	// Make a new bucket with '.' in its name, in 'us-west-2'. This
	// request is internally staged into a path style instead of
	// virtual host style.
	region = "us-west-2"
	args["region"] = region
	if err = c.MakeBucket(context.Background(), bucketName+".withperiod", minio.MakeBucketOptions{Region: region}); err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	// Delete all objects and buckets
	if err = cleanupBucket(bucketName+".withperiod", c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}
	logSuccess(testName, function, args, startTime)
}

// Test PutObject using a large data to trigger multipart readat
func testPutObjectReadAt() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "objectContentType",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	// Object content type
	objectContentType := "binary/octet-stream"
	args["objectContentType"] = objectContentType

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: objectContentType})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Get Object failed", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat Object failed", err)
		return
	}
	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Number of bytes in stat does not match, expected %d got %d", bufSize, st.Size), err)
		return
	}
	if st.ContentType != objectContentType && st.ContentType != "application/octet-stream" {
		logError(testName, function, args, startTime, "", "Content types don't match", err)
		return
	}
	if err := crcMatchesName(r, "datafile-129-MB"); err != nil {
		logError(testName, function, args, startTime, "", "data CRC check failed", err)
		return
	}
	if err := r.Close(); err != nil {
		logError(testName, function, args, startTime, "", "Object Close failed", err)
		return
	}
	if err := r.Close(); err == nil {
		logError(testName, function, args, startTime, "", "Object is already closed, didn't return error on Close", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testListObjectVersions() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ListObjectVersions(bucketName, prefix, recursive)"
	args := map[string]interface{}{
		"bucketName": "",
		"prefix":     "",
		"recursive":  "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	bufSize := dataFileMap["datafile-10-kB"]
	reader := getDataReader("datafile-10-kB")

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	reader.Close()

	bufSize = dataFileMap["datafile-1-b"]
	reader = getDataReader("datafile-1-b")
	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	reader.Close()

	err = c.RemoveObject(context.Background(), bucketName, objectName, minio.RemoveObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Unexpected object deletion", err)
		return
	}

	var deleteMarkers, versions int

	objectsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		if info.Key != objectName {
			logError(testName, function, args, startTime, "", "Unexpected object name in listing objects", nil)
			return
		}
		if info.VersionID == "" {
			logError(testName, function, args, startTime, "", "Unexpected version id in listing objects", nil)
			return
		}
		if info.IsDeleteMarker {
			deleteMarkers++
			if !info.IsLatest {
				logError(testName, function, args, startTime, "", "Unexpected IsLatest field in listing objects", nil)
				return
			}
		} else {
			versions++
		}
	}

	if deleteMarkers != 1 {
		logError(testName, function, args, startTime, "", "Unexpected number of DeleteMarker elements in listing objects", nil)
		return
	}

	if versions != 2 {
		logError(testName, function, args, startTime, "", "Unexpected number of Version elements in listing objects", nil)
		return
	}

	// Delete all objects and their versions as long as the bucket itself
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testStatObjectWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "StatObject"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	bufSize := dataFileMap["datafile-10-kB"]
	reader := getDataReader("datafile-10-kB")

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	reader.Close()

	bufSize = dataFileMap["datafile-1-b"]
	reader = getDataReader("datafile-1-b")
	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	reader.Close()

	objectsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})

	var results []minio.ObjectInfo
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		results = append(results, info)
	}

	if len(results) != 2 {
		logError(testName, function, args, startTime, "", "Unexpected number of Version elements in listing objects", nil)
		return
	}

	for i := 0; i < len(results); i++ {
		opts := minio.StatObjectOptions{VersionID: results[i].VersionID}
		statInfo, err := c.StatObject(context.Background(), bucketName, objectName, opts)
		if err != nil {
			logError(testName, function, args, startTime, "", "error during HEAD object", err)
			return
		}
		if statInfo.VersionID == "" || statInfo.VersionID != results[i].VersionID {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected version id", err)
			return
		}
		if statInfo.ETag != results[i].ETag {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected ETag", err)
			return
		}
		if statInfo.LastModified.Unix() != results[i].LastModified.Unix() {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected Last-Modified", err)
			return
		}
		if statInfo.Size != results[i].Size {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected Content-Length", err)
			return
		}
	}

	// Delete all objects and their versions as long as the bucket itself
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testGetObjectWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	// Save the contents of datafiles to check with GetObject() reader output later
	var buffers [][]byte
	testFiles := []string{"datafile-1-b", "datafile-10-kB"}

	for _, testFile := range testFiles {
		r := getDataReader(testFile)
		buf, err := io.ReadAll(r)
		if err != nil {
			logError(testName, function, args, startTime, "", "unexpected failure", err)
			return
		}
		r.Close()
		_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
		buffers = append(buffers, buf)
	}

	objectsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})

	var results []minio.ObjectInfo
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		results = append(results, info)
	}

	if len(results) != 2 {
		logError(testName, function, args, startTime, "", "Unexpected number of Version elements in listing objects", nil)
		return
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Size < results[j].Size
	})

	sort.SliceStable(buffers, func(i, j int) bool {
		return len(buffers[i]) < len(buffers[j])
	})

	for i := 0; i < len(results); i++ {
		opts := minio.GetObjectOptions{VersionID: results[i].VersionID}
		reader, err := c.GetObject(context.Background(), bucketName, objectName, opts)
		if err != nil {
			logError(testName, function, args, startTime, "", "error during  GET object", err)
			return
		}
		statInfo, err := reader.Stat()
		if err != nil {
			logError(testName, function, args, startTime, "", "error during calling reader.Stat()", err)
			return
		}
		if statInfo.ETag != results[i].ETag {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected ETag", err)
			return
		}
		if statInfo.LastModified.Unix() != results[i].LastModified.Unix() {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected Last-Modified", err)
			return
		}
		if statInfo.Size != results[i].Size {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected Content-Length", err)
			return
		}

		tmpBuffer := bytes.NewBuffer([]byte{})
		_, err = io.Copy(tmpBuffer, reader)
		if err != nil {
			logError(testName, function, args, startTime, "", "unexpected io.Copy()", err)
			return
		}

		if !bytes.Equal(tmpBuffer.Bytes(), buffers[i]) {
			logError(testName, function, args, startTime, "", "unexpected content of GetObject()", err)
			return
		}
	}

	// Delete all objects and their versions as long as the bucket itself
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testPutObjectWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	const n = 10
	// Read input...

	// Save the data concurrently.
	var wg sync.WaitGroup
	wg.Add(n)
	buffers := make([][]byte, n)
	var errs [n]error
	for i := 0; i < n; i++ {
		r := newRandomReader(int64((1<<20)*i+i), int64(i))
		buf, err := io.ReadAll(r)
		if err != nil {
			logError(testName, function, args, startTime, "", "unexpected failure", err)
			return
		}
		buffers[i] = buf

		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{PartSize: 5 << 20})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
	}

	objectsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})
	var results []minio.ObjectInfo
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", info.Err)
			return
		}
		results = append(results, info)
	}

	if len(results) != n {
		logError(testName, function, args, startTime, "", "Unexpected number of Version elements in listing objects", nil)
		return
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Size < results[j].Size
	})

	sort.Slice(buffers, func(i, j int) bool {
		return len(buffers[i]) < len(buffers[j])
	})

	for i := 0; i < len(results); i++ {
		opts := minio.GetObjectOptions{VersionID: results[i].VersionID}
		reader, err := c.GetObject(context.Background(), bucketName, objectName, opts)
		if err != nil {
			logError(testName, function, args, startTime, "", "error during  GET object", err)
			return
		}
		statInfo, err := reader.Stat()
		if err != nil {
			logError(testName, function, args, startTime, "", "error during calling reader.Stat()", err)
			return
		}
		if statInfo.ETag != results[i].ETag {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected ETag", err)
			return
		}
		if statInfo.LastModified.Unix() != results[i].LastModified.Unix() {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected Last-Modified", err)
			return
		}
		if statInfo.Size != results[i].Size {
			logError(testName, function, args, startTime, "", "error during HEAD object, unexpected Content-Length", err)
			return
		}

		tmpBuffer := bytes.NewBuffer([]byte{})
		_, err = io.Copy(tmpBuffer, reader)
		if err != nil {
			logError(testName, function, args, startTime, "", "unexpected io.Copy()", err)
			return
		}

		if !bytes.Equal(tmpBuffer.Bytes(), buffers[i]) {
			logError(testName, function, args, startTime, "", "unexpected content of GetObject()", err)
			return
		}
	}

	// Delete all objects and their versions as long as the bucket itself
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testListMultipartUpload() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}
	core := minio.Core{Client: c}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	ctx := context.Background()
	err = c.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}
	defer func() {
		if err = cleanupVersionedBucket(bucketName, c); err != nil {
			logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		}
	}()
	objName := "prefix/objectName"

	want := minio.ListMultipartUploadsResult{
		Bucket:             bucketName,
		KeyMarker:          "",
		UploadIDMarker:     "",
		NextKeyMarker:      "",
		NextUploadIDMarker: "",
		EncodingType:       "url",
		MaxUploads:         1000,
		IsTruncated:        false,
		Prefix:             "prefix/objectName",
		Delimiter:          "/",
		CommonPrefixes:     nil,
	}
	for i := 0; i < 5; i++ {
		uid, err := core.NewMultipartUpload(ctx, bucketName, objName, minio.PutObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "NewMultipartUpload failed", err)
			return
		}
		want.Uploads = append(want.Uploads, minio.ObjectMultipartInfo{
			Initiated:    time.Time{},
			StorageClass: "",
			Key:          objName,
			Size:         0,
			UploadID:     uid,
			Err:          nil,
		})

		for j := 0; j < 5; j++ {
			cmpGot := func(call string, got minio.ListMultipartUploadsResult) bool {
				for i := range got.Uploads {
					got.Uploads[i].Initiated = time.Time{}
				}
				if !reflect.DeepEqual(want, got) {
					err := fmt.Errorf("want: %#v\ngot : %#v", want, got)
					logError(testName, function, args, startTime, "", call+" failed", err)
				}
				return true
			}
			got, err := core.ListMultipartUploads(ctx, bucketName, objName, "", "", "/", 1000)
			if err != nil {
				logError(testName, function, args, startTime, "", "ListMultipartUploads failed", err)
				return
			}
			if !cmpGot("ListMultipartUploads-prefix", got) {
				return
			}
			got, err = core.ListMultipartUploads(ctx, bucketName, objName, objName, "", "/", 1000)
			got.KeyMarker = ""
			if err != nil {
				logError(testName, function, args, startTime, "", "ListMultipartUploads failed", err)
				return
			}
			if !cmpGot("ListMultipartUploads-marker", got) {
				return
			}
		}
		if i > 2 {
			err = core.AbortMultipartUpload(ctx, bucketName, objName, uid)
			if err != nil {
				logError(testName, function, args, startTime, "", "AbortMultipartUpload failed", err)
				return
			}
			want.Uploads = want.Uploads[:len(want.Uploads)-1]
		}
	}
	for _, up := range want.Uploads {
		err = core.AbortMultipartUpload(ctx, bucketName, objName, up.UploadID)
		if err != nil {
			logError(testName, function, args, startTime, "", "AbortMultipartUpload failed", err)
			return
		}
	}
	logSuccess(testName, function, args, startTime)
}

func testCopyObjectWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	testFiles := []string{"datafile-1-b", "datafile-10-kB"}
	for _, testFile := range testFiles {
		r := getDataReader(testFile)
		buf, err := io.ReadAll(r)
		if err != nil {
			logError(testName, function, args, startTime, "", "unexpected failure", err)
			return
		}
		r.Close()
		_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
	}

	objectsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})
	var infos []minio.ObjectInfo
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		infos = append(infos, info)
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Size < infos[j].Size
	})

	reader, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{VersionID: infos[0].VersionID})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject of the oldest version content failed", err)
		return
	}

	oldestContent, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "Reading the oldest object version failed", err)
		return
	}

	// Copy Source
	srcOpts := minio.CopySrcOptions{
		Bucket:    bucketName,
		Object:    objectName,
		VersionID: infos[0].VersionID,
	}
	args["src"] = srcOpts

	dstOpts := minio.CopyDestOptions{
		Bucket: bucketName,
		Object: objectName + "-copy",
	}
	args["dst"] = dstOpts

	// Perform the Copy
	if _, err = c.CopyObject(context.Background(), dstOpts, srcOpts); err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed", err)
		return
	}

	// Destination object
	readerCopy, err := c.GetObject(context.Background(), bucketName, objectName+"-copy", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	defer readerCopy.Close()

	newestContent, err := io.ReadAll(readerCopy)
	if err != nil {
		logError(testName, function, args, startTime, "", "Reading from GetObject reader failed", err)
		return
	}

	if len(newestContent) == 0 || !bytes.Equal(oldestContent, newestContent) {
		logError(testName, function, args, startTime, "", "Unexpected destination object content", err)
		return
	}

	// Delete all objects and their versions as long as the bucket itself
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testConcurrentCopyObjectWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	testFiles := []string{"datafile-10-kB"}
	for _, testFile := range testFiles {
		r := getDataReader(testFile)
		buf, err := io.ReadAll(r)
		if err != nil {
			logError(testName, function, args, startTime, "", "unexpected failure", err)
			return
		}
		r.Close()
		_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
	}

	objectsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})
	var infos []minio.ObjectInfo
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		infos = append(infos, info)
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Size < infos[j].Size
	})

	reader, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{VersionID: infos[0].VersionID})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject of the oldest version content failed", err)
		return
	}

	oldestContent, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "Reading the oldest object version failed", err)
		return
	}

	// Copy Source
	srcOpts := minio.CopySrcOptions{
		Bucket:    bucketName,
		Object:    objectName,
		VersionID: infos[0].VersionID,
	}
	args["src"] = srcOpts

	dstOpts := minio.CopyDestOptions{
		Bucket: bucketName,
		Object: objectName + "-copy",
	}
	args["dst"] = dstOpts

	// Perform the Copy concurrently
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	var errs [n]error
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.CopyObject(context.Background(), dstOpts, srcOpts)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			logError(testName, function, args, startTime, "", "CopyObject failed", err)
			return
		}
	}

	objectsInfo = c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: false, Prefix: dstOpts.Object})
	infos = []minio.ObjectInfo{}
	for info := range objectsInfo {
		// Destination object
		readerCopy, err := c.GetObject(context.Background(), bucketName, objectName+"-copy", minio.GetObjectOptions{VersionID: info.VersionID})
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObject failed", err)
			return
		}
		defer readerCopy.Close()

		newestContent, err := io.ReadAll(readerCopy)
		if err != nil {
			logError(testName, function, args, startTime, "", "Reading from GetObject reader failed", err)
			return
		}

		if len(newestContent) == 0 || !bytes.Equal(oldestContent, newestContent) {
			logError(testName, function, args, startTime, "", "Unexpected destination object content", err)
			return
		}
		infos = append(infos, info)
	}

	if len(infos) != n {
		logError(testName, function, args, startTime, "", "Unexpected number of Version elements in listing objects", nil)
		return
	}

	// Delete all objects and their versions as long as the bucket itself
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testComposeObjectWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ComposeObject()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	// var testFiles = []string{"datafile-5-MB", "datafile-10-kB"}
	testFiles := []string{"datafile-5-MB", "datafile-10-kB"}
	var testFilesBytes [][]byte

	for _, testFile := range testFiles {
		r := getDataReader(testFile)
		buf, err := io.ReadAll(r)
		if err != nil {
			logError(testName, function, args, startTime, "", "unexpected failure", err)
			return
		}
		r.Close()
		_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
		testFilesBytes = append(testFilesBytes, buf)
	}

	objectsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})

	var results []minio.ObjectInfo
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		results = append(results, info)
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Size > results[j].Size
	})

	// Source objects to concatenate. We also specify decryption
	// key for each
	src1 := minio.CopySrcOptions{
		Bucket:    bucketName,
		Object:    objectName,
		VersionID: results[0].VersionID,
	}

	src2 := minio.CopySrcOptions{
		Bucket:    bucketName,
		Object:    objectName,
		VersionID: results[1].VersionID,
	}

	dst := minio.CopyDestOptions{
		Bucket: bucketName,
		Object: objectName + "-copy",
	}

	_, err = c.ComposeObject(context.Background(), dst, src1, src2)
	if err != nil {
		logError(testName, function, args, startTime, "", "ComposeObject failed", err)
		return
	}

	// Destination object
	readerCopy, err := c.GetObject(context.Background(), bucketName, objectName+"-copy", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject of the copy object failed", err)
		return
	}
	defer readerCopy.Close()

	copyContentBytes, err := io.ReadAll(readerCopy)
	if err != nil {
		logError(testName, function, args, startTime, "", "Reading from the copy object reader failed", err)
		return
	}

	var expectedContent []byte
	for _, fileBytes := range testFilesBytes {
		expectedContent = append(expectedContent, fileBytes...)
	}

	if len(copyContentBytes) == 0 || !bytes.Equal(copyContentBytes, expectedContent) {
		logError(testName, function, args, startTime, "", "Unexpected destination object content", err)
		return
	}

	// Delete all objects and their versions as long as the bucket itself
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testRemoveObjectWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "DeleteObject()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, getDataReader("datafile-10-kB"), int64(dataFileMap["datafile-10-kB"]), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	objectsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})
	var version minio.ObjectInfo
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		version = info
		break
	}

	err = c.RemoveObject(context.Background(), bucketName, objectName, minio.RemoveObjectOptions{VersionID: version.VersionID})
	if err != nil {
		logError(testName, function, args, startTime, "", "DeleteObject failed", err)
		return
	}

	objectsInfo = c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})
	for range objectsInfo {
		logError(testName, function, args, startTime, "", "Unexpected versioning info, should not have any one ", err)
		return
	}
	// test delete marker version id is non-null
	_, err = c.PutObject(context.Background(), bucketName, objectName, getDataReader("datafile-10-kB"), int64(dataFileMap["datafile-10-kB"]), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	// create delete marker
	err = c.RemoveObject(context.Background(), bucketName, objectName, minio.RemoveObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "DeleteObject failed", err)
		return
	}
	objectsInfo = c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})
	idx := 0
	for info := range objectsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		if idx == 0 {
			if !info.IsDeleteMarker {
				logError(testName, function, args, startTime, "", "Unexpected error - expected delete marker to have been created", err)
				return
			}
			if info.VersionID == "" {
				logError(testName, function, args, startTime, "", "Unexpected error - expected delete marker to be versioned", err)
				return
			}
		}
		idx++
	}

	defer cleanupBucket(bucketName, c)

	logSuccess(testName, function, args, startTime)
}

func testRemoveObjectsWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "DeleteObjects()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, getDataReader("datafile-10-kB"), int64(dataFileMap["datafile-10-kB"]), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	objectsVersions := make(chan minio.ObjectInfo)
	go func() {
		objectsVersionsInfo := c.ListObjects(context.Background(), bucketName,
			minio.ListObjectsOptions{WithVersions: true, Recursive: true})
		for info := range objectsVersionsInfo {
			if info.Err != nil {
				logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
				return
			}
			objectsVersions <- info
		}
		close(objectsVersions)
	}()

	removeErrors := c.RemoveObjects(context.Background(), bucketName, objectsVersions, minio.RemoveObjectsOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "DeleteObjects call failed", err)
		return
	}

	for e := range removeErrors {
		if e.Err != nil {
			logError(testName, function, args, startTime, "", "Single delete operation failed", err)
			return
		}
	}

	objectsVersionsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})
	for range objectsVersionsInfo {
		logError(testName, function, args, startTime, "", "Unexpected versioning info, should not have any one ", err)
		return
	}

	err = c.RemoveBucket(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testObjectTaggingWithVersioning() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "{Get,Set,Remove}ObjectTagging()"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	err = c.EnableVersioning(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "Enable versioning failed", err)
		return
	}

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	for _, file := range []string{"datafile-1-b", "datafile-10-kB"} {
		_, err = c.PutObject(context.Background(), bucketName, objectName, getDataReader(file), int64(dataFileMap[file]), minio.PutObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
	}

	versionsInfo := c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{WithVersions: true, Recursive: true})

	var versions []minio.ObjectInfo
	for info := range versionsInfo {
		if info.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error during listing objects", err)
			return
		}
		versions = append(versions, info)
	}

	sort.SliceStable(versions, func(i, j int) bool {
		return versions[i].Size < versions[j].Size
	})

	tagsV1 := map[string]string{"key1": "val1"}
	t1, err := tags.MapToObjectTags(tagsV1)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectTagging (1) failed", err)
		return
	}

	err = c.PutObjectTagging(context.Background(), bucketName, objectName, t1, minio.PutObjectTaggingOptions{VersionID: versions[0].VersionID})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectTagging (1) failed", err)
		return
	}

	tagsV2 := map[string]string{"key2": "val2"}
	t2, err := tags.MapToObjectTags(tagsV2)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectTagging (1) failed", err)
		return
	}

	err = c.PutObjectTagging(context.Background(), bucketName, objectName, t2, minio.PutObjectTaggingOptions{VersionID: versions[1].VersionID})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectTagging (2) failed", err)
		return
	}

	tagsEqual := func(tags1, tags2 map[string]string) bool {
		for k1, v1 := range tags1 {
			v2, found := tags2[k1]
			if found {
				if v1 != v2 {
					return false
				}
			}
		}
		return true
	}

	gotTagsV1, err := c.GetObjectTagging(context.Background(), bucketName, objectName, minio.GetObjectTaggingOptions{VersionID: versions[0].VersionID})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObjectTagging failed", err)
		return
	}

	if !tagsEqual(t1.ToMap(), gotTagsV1.ToMap()) {
		logError(testName, function, args, startTime, "", "Unexpected tags content (1)", err)
		return
	}

	gotTagsV2, err := c.GetObjectTagging(context.Background(), bucketName, objectName, minio.GetObjectTaggingOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObjectTaggingContext failed", err)
		return
	}

	if !tagsEqual(t2.ToMap(), gotTagsV2.ToMap()) {
		logError(testName, function, args, startTime, "", "Unexpected tags content (2)", err)
		return
	}

	err = c.RemoveObjectTagging(context.Background(), bucketName, objectName, minio.RemoveObjectTaggingOptions{VersionID: versions[0].VersionID})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectTagging (2) failed", err)
		return
	}

	emptyTags, err := c.GetObjectTagging(context.Background(), bucketName, objectName,
		minio.GetObjectTaggingOptions{VersionID: versions[0].VersionID})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObjectTagging failed", err)
		return
	}

	if len(emptyTags.ToMap()) != 0 {
		logError(testName, function, args, startTime, "", "Unexpected tags content (2)", err)
		return
	}

	// Delete all objects and their versions as long as the bucket itself
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test PutObject with custom checksums.
func testPutObjectWithChecksums() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader,size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.PutObjectOptions{UserMetadata: metadata, Progress: progress}",
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)
	tests := []struct {
		cs minio.ChecksumType
	}{
		{cs: minio.ChecksumCRC32C},
		{cs: minio.ChecksumCRC32},
		{cs: minio.ChecksumSHA1},
		{cs: minio.ChecksumSHA256},
		{cs: minio.ChecksumCRC64NVME},
	}

	for _, test := range tests {
		if os.Getenv("MINT_NO_FULL_OBJECT") != "" && test.cs.FullObjectRequested() {
			continue
		}
		bufSize := dataFileMap["datafile-10-kB"]

		// Save the data
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName

		cmpChecksum := func(got, want string) {
			if want != got {
				logError(testName, function, args, startTime, "", "checksum mismatch", fmt.Errorf("want %s, got %s", want, got))
				return
			}
		}

		meta := map[string]string{}
		reader := getDataReader("datafile-10-kB")
		b, err := io.ReadAll(reader)
		if err != nil {
			logError(testName, function, args, startTime, "", "Read failed", err)
			return
		}
		h := test.cs.Hasher()
		h.Reset()

		// Test with a bad CRC - we haven't called h.Write(b), so this is a checksum of empty data
		meta[test.cs.Key()] = base64.StdEncoding.EncodeToString(h.Sum(nil))
		args["metadata"] = meta
		args["range"] = "false"
		args["checksum"] = test.cs.String()

		resp, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(b), int64(bufSize), minio.PutObjectOptions{
			DisableMultipart: true,
			UserMetadata:     meta,
		})
		if err == nil {
			logError(testName, function, args, startTime, "", "PutObject did not fail on wrong CRC", err)
			return
		}

		// Set correct CRC.
		h.Write(b)
		meta[test.cs.Key()] = base64.StdEncoding.EncodeToString(h.Sum(nil))
		reader.Close()

		resp, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(b), int64(bufSize), minio.PutObjectOptions{
			DisableMultipart:     true,
			DisableContentSha256: true,
			UserMetadata:         meta,
		})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
		cmpChecksum(resp.ChecksumSHA256, meta["x-amz-checksum-sha256"])
		cmpChecksum(resp.ChecksumSHA1, meta["x-amz-checksum-sha1"])
		cmpChecksum(resp.ChecksumCRC32, meta["x-amz-checksum-crc32"])
		cmpChecksum(resp.ChecksumCRC32C, meta["x-amz-checksum-crc32c"])
		cmpChecksum(resp.ChecksumCRC64NVME, meta["x-amz-checksum-crc64nvme"])

		// Read the data back
		gopts := minio.GetObjectOptions{Checksum: true}

		r, err := c.GetObject(context.Background(), bucketName, objectName, gopts)
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObject failed", err)
			return
		}

		st, err := r.Stat()
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}
		cmpChecksum(st.ChecksumSHA256, meta["x-amz-checksum-sha256"])
		cmpChecksum(st.ChecksumSHA1, meta["x-amz-checksum-sha1"])
		cmpChecksum(st.ChecksumCRC32, meta["x-amz-checksum-crc32"])
		cmpChecksum(st.ChecksumCRC32C, meta["x-amz-checksum-crc32c"])
		cmpChecksum(st.ChecksumCRC64NVME, meta["x-amz-checksum-crc64nvme"])

		if st.Size != int64(bufSize) {
			logError(testName, function, args, startTime, "", "Number of bytes returned by PutObject does not match GetObject, expected "+string(bufSize)+" got "+string(st.Size), err)
			return
		}

		if err := r.Close(); err != nil {
			logError(testName, function, args, startTime, "", "Object Close failed", err)
			return
		}
		if err := r.Close(); err == nil {
			logError(testName, function, args, startTime, "", "Object already closed, should respond with error", err)
			return
		}

		args["range"] = "true"
		err = gopts.SetRange(100, 1000)
		if err != nil {
			logError(testName, function, args, startTime, "", "SetRange failed", err)
			return
		}
		r, err = c.GetObject(context.Background(), bucketName, objectName, gopts)
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObject failed", err)
			return
		}

		b, err = io.ReadAll(r)
		if err != nil {
			logError(testName, function, args, startTime, "", "Read failed", err)
			return
		}
		st, err = r.Stat()
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}

		// Range requests should return empty checksums...
		cmpChecksum(st.ChecksumSHA256, "")
		cmpChecksum(st.ChecksumSHA1, "")
		cmpChecksum(st.ChecksumCRC32, "")
		cmpChecksum(st.ChecksumCRC32C, "")
		cmpChecksum(st.ChecksumCRC64NVME, "")

		delete(args, "range")
		delete(args, "metadata")
		logSuccess(testName, function, args, startTime)
	}
}

// Test PutObject with custom checksums.
func testPutObjectWithTrailingChecksums() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader,size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.PutObjectOptions{UserMetadata: metadata, Progress: progress, TrailChecksum: xxx}",
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{TrailingHeaders: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)
	tests := []struct {
		cs minio.ChecksumType
	}{
		{cs: minio.ChecksumCRC64NVME},
		{cs: minio.ChecksumCRC32C},
		{cs: minio.ChecksumCRC32},
		{cs: minio.ChecksumSHA1},
		{cs: minio.ChecksumSHA256},
	}
	for _, test := range tests {
		if os.Getenv("MINT_NO_FULL_OBJECT") != "" && test.cs.FullObjectRequested() {
			continue
		}
		function := "PutObject(bucketName, objectName, reader,size, opts)"
		bufSize := dataFileMap["datafile-10-kB"]

		// Save the data
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName

		cmpChecksum := func(got, want string) {
			if want != got {
				logError(testName, function, args, startTime, "", "checksum mismatch", fmt.Errorf("want %s, got %s", want, got))
				return
			}
		}

		meta := map[string]string{}
		reader := getDataReader("datafile-10-kB")
		b, err := io.ReadAll(reader)
		if err != nil {
			logError(testName, function, args, startTime, "", "Read failed", err)
			return
		}
		h := test.cs.Hasher()
		h.Reset()

		// Test with Wrong CRC.
		args["metadata"] = meta
		args["range"] = "false"
		args["checksum"] = test.cs.String()

		resp, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(b), int64(bufSize), minio.PutObjectOptions{
			DisableMultipart:     true,
			DisableContentSha256: true,
			UserMetadata:         meta,
			Checksum:             test.cs,
		})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}

		h.Write(b)
		meta[test.cs.Key()] = base64.StdEncoding.EncodeToString(h.Sum(nil))

		cmpChecksum(resp.ChecksumSHA256, meta["x-amz-checksum-sha256"])
		cmpChecksum(resp.ChecksumSHA1, meta["x-amz-checksum-sha1"])
		cmpChecksum(resp.ChecksumCRC32, meta["x-amz-checksum-crc32"])
		cmpChecksum(resp.ChecksumCRC32C, meta["x-amz-checksum-crc32c"])
		cmpChecksum(resp.ChecksumCRC64NVME, meta["x-amz-checksum-crc64nvme"])

		// Read the data back
		gopts := minio.GetObjectOptions{Checksum: true}

		function = "GetObject(...)"
		r, err := c.GetObject(context.Background(), bucketName, objectName, gopts)
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObject failed", err)
			return
		}

		st, err := r.Stat()
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}
		cmpChecksum(st.ChecksumSHA256, meta["x-amz-checksum-sha256"])
		cmpChecksum(st.ChecksumSHA1, meta["x-amz-checksum-sha1"])
		cmpChecksum(st.ChecksumCRC32, meta["x-amz-checksum-crc32"])
		cmpChecksum(st.ChecksumCRC32C, meta["x-amz-checksum-crc32c"])
		cmpChecksum(resp.ChecksumCRC64NVME, meta["x-amz-checksum-crc64nvme"])

		if st.Size != int64(bufSize) {
			logError(testName, function, args, startTime, "", "Number of bytes returned by PutObject does not match GetObject, expected "+string(bufSize)+" got "+string(st.Size), err)
			return
		}

		if err := r.Close(); err != nil {
			logError(testName, function, args, startTime, "", "Object Close failed", err)
			return
		}
		if err := r.Close(); err == nil {
			logError(testName, function, args, startTime, "", "Object already closed, should respond with error", err)
			return
		}

		function = "GetObject( Range...)"
		args["range"] = "true"
		err = gopts.SetRange(100, 1000)
		if err != nil {
			logError(testName, function, args, startTime, "", "SetRange failed", err)
			return
		}
		r, err = c.GetObject(context.Background(), bucketName, objectName, gopts)
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObject failed", err)
			return
		}

		b, err = io.ReadAll(r)
		if err != nil {
			logError(testName, function, args, startTime, "", "Read failed", err)
			return
		}
		st, err = r.Stat()
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}

		// Range requests should return empty checksums...
		cmpChecksum(st.ChecksumSHA256, "")
		cmpChecksum(st.ChecksumSHA1, "")
		cmpChecksum(st.ChecksumCRC32, "")
		cmpChecksum(st.ChecksumCRC32C, "")
		cmpChecksum(st.ChecksumCRC64NVME, "")

		function = "GetObjectAttributes(...)"
		s, err := c.GetObjectAttributes(context.Background(), bucketName, objectName, minio.ObjectAttributesOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObjectAttributes failed", err)
			return
		}
		cmpChecksum(s.Checksum.ChecksumSHA256, meta["x-amz-checksum-sha256"])
		cmpChecksum(s.Checksum.ChecksumSHA1, meta["x-amz-checksum-sha1"])
		cmpChecksum(s.Checksum.ChecksumCRC32, meta["x-amz-checksum-crc32"])
		cmpChecksum(s.Checksum.ChecksumCRC32C, meta["x-amz-checksum-crc32c"])

		delete(args, "range")
		delete(args, "metadata")
		logSuccess(testName, function, args, startTime)
	}
}

// Test PutObject with custom checksums.
func testPutMultipartObjectWithChecksums(trailing bool) {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader,size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       fmt.Sprintf("minio.PutObjectOptions{UserMetadata: metadata, Trailing: %v}", trailing),
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{TrailingHeaders: trailing})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	hashMultiPart := func(b []byte, partSize int, cs minio.ChecksumType) string {
		r := bytes.NewReader(b)
		hasher := cs.Hasher()
		if cs.FullObjectRequested() {
			partSize = len(b)
		}
		tmp := make([]byte, partSize)
		parts := 0
		var all []byte
		for {
			n, err := io.ReadFull(r, tmp)
			if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
				logError(testName, function, args, startTime, "", "Calc crc failed", err)
			}
			if n == 0 {
				break
			}
			parts++
			hasher.Reset()
			hasher.Write(tmp[:n])
			all = append(all, hasher.Sum(nil)...)
			if err != nil {
				break
			}
		}
		if parts == 1 {
			return base64.StdEncoding.EncodeToString(hasher.Sum(nil))
		}
		hasher.Reset()
		hasher.Write(all)
		return fmt.Sprintf("%s-%d", base64.StdEncoding.EncodeToString(hasher.Sum(nil)), parts)
	}
	defer cleanupBucket(bucketName, c)
	tests := []struct {
		cs minio.ChecksumType
	}{
		{cs: minio.ChecksumFullObjectCRC32},
		{cs: minio.ChecksumFullObjectCRC32C},
		{cs: minio.ChecksumCRC64NVME},
		{cs: minio.ChecksumCRC32C},
		{cs: minio.ChecksumCRC32},
		{cs: minio.ChecksumSHA1},
		{cs: minio.ChecksumSHA256},
	}

	for _, test := range tests {
		if os.Getenv("MINT_NO_FULL_OBJECT") != "" && test.cs.FullObjectRequested() {
			continue
		}

		args["section"] = "prep"
		bufSize := dataFileMap["datafile-129-MB"]
		// Save the data
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName
		args["checksum"] = test.cs.String()

		cmpChecksum := func(got, want string) {
			if want != got {
				logError(testName, function, args, startTime, "", "checksum mismatch", fmt.Errorf("want %s, got %s", want, got))
				// fmt.Printf("want %s, got %s\n", want, got)
				return
			}
		}

		const partSize = 10 << 20
		reader := getDataReader("datafile-129-MB")
		b, err := io.ReadAll(reader)
		if err != nil {
			logError(testName, function, args, startTime, "", "Read failed", err)
			return
		}
		reader.Close()
		h := test.cs.Hasher()
		h.Reset()
		want := hashMultiPart(b, partSize, test.cs)

		var cs minio.ChecksumType
		rd := io.Reader(io.NopCloser(bytes.NewReader(b)))
		if trailing {
			cs = test.cs
			rd = bytes.NewReader(b)
		}

		// Set correct CRC.
		args["section"] = "PutObject"
		resp, err := c.PutObject(context.Background(), bucketName, objectName, rd, int64(bufSize), minio.PutObjectOptions{
			DisableContentSha256: true,
			DisableMultipart:     false,
			UserMetadata:         nil,
			PartSize:             partSize,
			AutoChecksum:         test.cs,
			Checksum:             cs,
		})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}

		switch test.cs.Base() {
		case minio.ChecksumCRC32C:
			cmpChecksum(resp.ChecksumCRC32C, want)
		case minio.ChecksumCRC32:
			cmpChecksum(resp.ChecksumCRC32, want)
		case minio.ChecksumSHA1:
			cmpChecksum(resp.ChecksumSHA1, want)
		case minio.ChecksumSHA256:
			cmpChecksum(resp.ChecksumSHA256, want)
		case minio.ChecksumCRC64NVME:
			cmpChecksum(resp.ChecksumCRC64NVME, want)
		}

		args["section"] = "HeadObject"
		st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{Checksum: true})
		if err != nil {
			logError(testName, function, args, startTime, "", "StatObject failed", err)
			return
		}
		switch test.cs.Base() {
		case minio.ChecksumCRC32C:
			cmpChecksum(st.ChecksumCRC32C, want)
		case minio.ChecksumCRC32:
			cmpChecksum(st.ChecksumCRC32, want)
		case minio.ChecksumSHA1:
			cmpChecksum(st.ChecksumSHA1, want)
		case minio.ChecksumSHA256:
			cmpChecksum(st.ChecksumSHA256, want)
		case minio.ChecksumCRC64NVME:
			cmpChecksum(st.ChecksumCRC64NVME, want)
		}

		args["section"] = "GetObjectAttributes"
		s, err := c.GetObjectAttributes(context.Background(), bucketName, objectName, minio.ObjectAttributesOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObjectAttributes failed", err)
			return
		}

		if strings.ContainsRune(want, '-') {
			want = want[:strings.IndexByte(want, '-')]
		}
		switch test.cs {
		// Full Object CRC does not return anything with GetObjectAttributes
		case minio.ChecksumCRC32C:
			cmpChecksum(s.Checksum.ChecksumCRC32C, want)
		case minio.ChecksumCRC32:
			cmpChecksum(s.Checksum.ChecksumCRC32, want)
		case minio.ChecksumSHA1:
			cmpChecksum(s.Checksum.ChecksumSHA1, want)
		case minio.ChecksumSHA256:
			cmpChecksum(s.Checksum.ChecksumSHA256, want)
		}

		// Read the data back
		gopts := minio.GetObjectOptions{Checksum: true}
		gopts.PartNumber = 2

		// We cannot use StatObject, since it ignores partnumber.
		args["section"] = "GetObject-Part"
		r, err := c.GetObject(context.Background(), bucketName, objectName, gopts)
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObject failed", err)
			return
		}
		io.Copy(io.Discard, r)
		st, err = r.Stat()
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}

		// Test part 2 checksum...
		h.Reset()
		h.Write(b[partSize : 2*partSize])
		want = base64.StdEncoding.EncodeToString(h.Sum(nil))

		switch test.cs {
		// Full Object CRC does not return any part CRC for whatever reason.
		case minio.ChecksumCRC32C:
			cmpChecksum(st.ChecksumCRC32C, want)
		case minio.ChecksumCRC32:
			cmpChecksum(st.ChecksumCRC32, want)
		case minio.ChecksumSHA1:
			cmpChecksum(st.ChecksumSHA1, want)
		case minio.ChecksumSHA256:
			cmpChecksum(st.ChecksumSHA256, want)
		case minio.ChecksumCRC64NVME:
			// AWS doesn't return part checksum, but may in the future.
			if st.ChecksumCRC64NVME != "" {
				cmpChecksum(st.ChecksumCRC64NVME, want)
			}
		}

		delete(args, "metadata")
		delete(args, "section")
		logSuccess(testName, function, args, startTime)
	}
}

// Test PutObject with trailing checksums.
func testTrailingChecksums() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader,size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.PutObjectOptions{UserMetadata: metadata, Progress: progress}",
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{TrailingHeaders: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	hashMultiPart := func(b []byte, partSize int, hasher hash.Hash) string {
		r := bytes.NewReader(b)
		tmp := make([]byte, partSize)
		parts := 0
		var all []byte
		for {
			n, err := io.ReadFull(r, tmp)
			if err != nil && err != io.ErrUnexpectedEOF {
				logError(testName, function, args, startTime, "", "Calc crc failed", err)
			}
			if n == 0 {
				break
			}
			parts++
			hasher.Reset()
			hasher.Write(tmp[:n])
			all = append(all, hasher.Sum(nil)...)
			if err != nil {
				break
			}
		}
		hasher.Reset()
		hasher.Write(all)
		return fmt.Sprintf("%s-%d", base64.StdEncoding.EncodeToString(hasher.Sum(nil)), parts)
	}
	defer cleanupBucket(bucketName, c)
	tests := []struct {
		header string
		hasher hash.Hash

		// Checksum values
		ChecksumCRC32  string
		ChecksumCRC32C string
		ChecksumSHA1   string
		ChecksumSHA256 string
		PO             minio.PutObjectOptions
	}{
		// Currently there is no way to override the checksum type.
		{
			header:         "x-amz-checksum-crc32c",
			hasher:         crc32.New(crc32.MakeTable(crc32.Castagnoli)),
			ChecksumCRC32C: "set",
			PO: minio.PutObjectOptions{
				DisableContentSha256: true,
				DisableMultipart:     false,
				UserMetadata:         nil,
				PartSize:             5 << 20,
			},
		},
		{
			header:         "x-amz-checksum-crc32c",
			hasher:         crc32.New(crc32.MakeTable(crc32.Castagnoli)),
			ChecksumCRC32C: "set",
			PO: minio.PutObjectOptions{
				DisableContentSha256: true,
				DisableMultipart:     false,
				UserMetadata:         nil,
				PartSize:             6_645_654, // Rather arbitrary size
			},
		},
		{
			header:         "x-amz-checksum-crc32c",
			hasher:         crc32.New(crc32.MakeTable(crc32.Castagnoli)),
			ChecksumCRC32C: "set",
			PO: minio.PutObjectOptions{
				DisableContentSha256: false,
				DisableMultipart:     false,
				UserMetadata:         nil,
				PartSize:             5 << 20,
			},
		},
		{
			header:         "x-amz-checksum-crc32c",
			hasher:         crc32.New(crc32.MakeTable(crc32.Castagnoli)),
			ChecksumCRC32C: "set",
			PO: minio.PutObjectOptions{
				DisableContentSha256: false,
				DisableMultipart:     false,
				UserMetadata:         nil,
				PartSize:             6_645_654, // Rather arbitrary size
			},
		},
	}

	for _, test := range tests {
		bufSize := dataFileMap["datafile-11-MB"]

		// Save the data
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName

		cmpChecksum := func(got, want string) {
			if want != got {
				logError(testName, function, args, startTime, "", "checksum mismatch", fmt.Errorf("want %q, got %q", want, got))
				return
			}
		}

		reader := getDataReader("datafile-11-MB")
		b, err := io.ReadAll(reader)
		if err != nil {
			logError(testName, function, args, startTime, "", "Read failed", err)
			return
		}
		reader.Close()
		h := test.hasher
		h.Reset()
		test.ChecksumCRC32C = hashMultiPart(b, int(test.PO.PartSize), test.hasher)

		// Set correct CRC.
		resp, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(b), int64(bufSize), test.PO)
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
		// c.TraceOff()
		cmpChecksum(resp.ChecksumSHA256, test.ChecksumSHA256)
		cmpChecksum(resp.ChecksumSHA1, test.ChecksumSHA1)
		cmpChecksum(resp.ChecksumCRC32, test.ChecksumCRC32)
		cmpChecksum(resp.ChecksumCRC32C, test.ChecksumCRC32C)

		// Read the data back
		gopts := minio.GetObjectOptions{Checksum: true}
		gopts.PartNumber = 2

		// We cannot use StatObject, since it ignores partnumber.
		r, err := c.GetObject(context.Background(), bucketName, objectName, gopts)
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObject failed", err)
			return
		}
		io.Copy(io.Discard, r)
		st, err := r.Stat()
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}

		// Test part 2 checksum...
		h.Reset()
		p2 := b[test.PO.PartSize:]
		if len(p2) > int(test.PO.PartSize) {
			p2 = p2[:test.PO.PartSize]
		}
		h.Write(p2)
		got := base64.StdEncoding.EncodeToString(h.Sum(nil))
		if test.ChecksumSHA256 != "" {
			cmpChecksum(st.ChecksumSHA256, got)
		}
		if test.ChecksumSHA1 != "" {
			cmpChecksum(st.ChecksumSHA1, got)
		}
		if test.ChecksumCRC32 != "" {
			cmpChecksum(st.ChecksumCRC32, got)
		}
		if test.ChecksumCRC32C != "" {
			cmpChecksum(st.ChecksumCRC32C, got)
		}

		delete(args, "metadata")
		logSuccess(testName, function, args, startTime)
	}
}

// Test PutObject with custom checksums.
func testPutObjectWithAutomaticChecksums() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader,size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.PutObjectOptions{UserMetadata: metadata, Progress: progress}",
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{TrailingHeaders: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)
	tests := []struct {
		header string
		hasher hash.Hash

		// Checksum values
		ChecksumCRC32  string
		ChecksumCRC32C string
		ChecksumSHA1   string
		ChecksumSHA256 string
	}{
		// Built-in will only add crc32c, when no MD5 nor SHA256.
		{header: "x-amz-checksum-crc32c", hasher: crc32.New(crc32.MakeTable(crc32.Castagnoli))},
	}

	// defer c.TraceOff()

	for i, test := range tests {
		bufSize := dataFileMap["datafile-10-kB"]

		// Save the data
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName

		cmpChecksum := func(got, want string) {
			if want != got {
				logError(testName, function, args, startTime, "", "checksum mismatch", fmt.Errorf("want %s, got %s", want, got))
				return
			}
		}

		meta := map[string]string{}
		reader := getDataReader("datafile-10-kB")
		b, err := io.ReadAll(reader)
		if err != nil {
			logError(testName, function, args, startTime, "", "Read failed", err)
			return
		}

		h := test.hasher
		h.Reset()
		h.Write(b)
		meta[test.header] = base64.StdEncoding.EncodeToString(h.Sum(nil))
		args["metadata"] = meta

		resp, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(b), int64(bufSize), minio.PutObjectOptions{
			DisableMultipart:     true,
			UserMetadata:         nil,
			DisableContentSha256: true,
			SendContentMd5:       false,
		})
		if err == nil {
			if i == 0 && resp.ChecksumCRC32C == "" {
				logIgnored(testName, function, args, startTime, "Checksums does not appear to be supported by backend")
				return
			}
		} else {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
		cmpChecksum(resp.ChecksumSHA256, meta["x-amz-checksum-sha256"])
		cmpChecksum(resp.ChecksumSHA1, meta["x-amz-checksum-sha1"])
		cmpChecksum(resp.ChecksumCRC32, meta["x-amz-checksum-crc32"])
		cmpChecksum(resp.ChecksumCRC32C, meta["x-amz-checksum-crc32c"])

		// Usually this will be the same as above, since we skip automatic checksum when SHA256 content is sent.
		// When/if we add a checksum control to PutObjectOptions this will make more sense.
		resp, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(b), int64(bufSize), minio.PutObjectOptions{
			DisableMultipart:     true,
			UserMetadata:         nil,
			DisableContentSha256: false,
			SendContentMd5:       false,
		})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
		// The checksum will not be enabled on HTTP, since it uses SHA256 blocks.
		if mustParseBool(os.Getenv(enableHTTPS)) {
			cmpChecksum(resp.ChecksumSHA256, meta["x-amz-checksum-sha256"])
			cmpChecksum(resp.ChecksumSHA1, meta["x-amz-checksum-sha1"])
			cmpChecksum(resp.ChecksumCRC32, meta["x-amz-checksum-crc32"])
			cmpChecksum(resp.ChecksumCRC32C, meta["x-amz-checksum-crc32c"])
		}

		// Set SHA256 header manually
		sh256 := sha256.Sum256(b)
		meta = map[string]string{"x-amz-checksum-sha256": base64.StdEncoding.EncodeToString(sh256[:])}
		args["metadata"] = meta
		resp, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(b), int64(bufSize), minio.PutObjectOptions{
			DisableMultipart:     true,
			UserMetadata:         meta,
			DisableContentSha256: true,
			SendContentMd5:       false,
		})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
		cmpChecksum(resp.ChecksumSHA256, meta["x-amz-checksum-sha256"])
		cmpChecksum(resp.ChecksumSHA1, meta["x-amz-checksum-sha1"])
		cmpChecksum(resp.ChecksumCRC32, meta["x-amz-checksum-crc32"])
		cmpChecksum(resp.ChecksumCRC32C, meta["x-amz-checksum-crc32c"])
		delete(args, "metadata")
	}

	logSuccess(testName, function, args, startTime)
}

func testGetObjectAttributes() {
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObjectAttributes(ctx, bucketName, objectName, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.ObjectAttributesOptions{}",
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{TrailingHeaders: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName
	err = c.MakeBucket(
		context.Background(),
		bucketName,
		minio.MakeBucketOptions{Region: "us-east-1"},
	)
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	bucketNameV := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-versioned-")
	args["bucketName"] = bucketNameV
	err = c.MakeBucket(
		context.Background(),
		bucketNameV,
		minio.MakeBucketOptions{Region: "us-east-1"},
	)
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}
	err = c.EnableVersioning(context.Background(), bucketNameV)
	if err != nil {
		logError(testName, function, args, startTime, "", "Unable to enable versioning", err)
		return
	}

	defer cleanupBucket(bucketName, c)
	defer cleanupVersionedBucket(bucketNameV, c)

	testFiles := make(map[string]*objectAttributesNewObject)
	testFiles["file1"] = &objectAttributesNewObject{
		Object:           "file1",
		ObjectReaderType: "datafile-1.03-MB",
		Bucket:           bucketNameV,
		ContentType:      "custom/contenttype",
		SendContentMd5:   false,
	}

	testFiles["file2"] = &objectAttributesNewObject{
		Object:           "file2",
		ObjectReaderType: "datafile-129-MB",
		Bucket:           bucketName,
		ContentType:      "custom/contenttype",
		SendContentMd5:   false,
	}

	for i, v := range testFiles {
		bufSize := dataFileMap[v.ObjectReaderType]

		reader := getDataReader(v.ObjectReaderType)

		args["objectName"] = v.Object
		testFiles[i].UploadInfo, err = c.PutObject(context.Background(), v.Bucket, v.Object, reader, int64(bufSize), minio.PutObjectOptions{
			ContentType:    v.ContentType,
			SendContentMd5: v.SendContentMd5,
			Checksum:       minio.ChecksumCRC32C,
		})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObject failed", err)
			return
		}
	}

	testTable := make(map[string]objectAttributesTableTest)

	testTable["none-versioned"] = objectAttributesTableTest{
		opts: minio.ObjectAttributesOptions{},
		test: objectAttributesTestOptions{
			TestFileName:     "file2",
			StorageClass:     "STANDARD",
			HasFullChecksum:  true,
			HasPartChecksums: true,
			HasParts:         true,
		},
	}

	testTable["0-to-0-marker"] = objectAttributesTableTest{
		opts: minio.ObjectAttributesOptions{
			PartNumberMarker: 0,
			MaxParts:         0,
		},
		test: objectAttributesTestOptions{
			TestFileName:     "file2",
			StorageClass:     "STANDARD",
			HasFullChecksum:  true,
			HasPartChecksums: true,
			HasParts:         true,
		},
	}

	testTable["0-marker-to-max"] = objectAttributesTableTest{
		opts: minio.ObjectAttributesOptions{
			PartNumberMarker: 0,
			MaxParts:         10000,
		},
		test: objectAttributesTestOptions{
			TestFileName:     "file2",
			StorageClass:     "STANDARD",
			HasFullChecksum:  true,
			HasPartChecksums: true,
			HasParts:         true,
		},
	}

	testTable["0-to-1-marker"] = objectAttributesTableTest{
		opts: minio.ObjectAttributesOptions{
			PartNumberMarker: 0,
			MaxParts:         1,
		},
		test: objectAttributesTestOptions{
			TestFileName:     "file2",
			StorageClass:     "STANDARD",
			HasFullChecksum:  true,
			HasPartChecksums: true,
			HasParts:         true,
		},
	}

	testTable["7-to-6-marker"] = objectAttributesTableTest{
		opts: minio.ObjectAttributesOptions{
			PartNumberMarker: 7,
			MaxParts:         6,
		},
		test: objectAttributesTestOptions{
			TestFileName:     "file2",
			StorageClass:     "STANDARD",
			HasFullChecksum:  true,
			HasPartChecksums: true,
			HasParts:         true,
		},
	}

	testTable["versioned"] = objectAttributesTableTest{
		opts: minio.ObjectAttributesOptions{},
		test: objectAttributesTestOptions{
			TestFileName:    "file1",
			StorageClass:    "STANDARD",
			HasFullChecksum: true,
		},
	}

	for i, v := range testTable {

		tf, ok := testFiles[v.test.TestFileName]
		if !ok {
			continue
		}

		args["objectName"] = tf.Object
		args["bucketName"] = tf.Bucket
		if tf.UploadInfo.VersionID != "" {
			v.opts.VersionID = tf.UploadInfo.VersionID
		}

		s, err := c.GetObjectAttributes(context.Background(), tf.Bucket, tf.Object, v.opts)
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObjectAttributes failed", err)
			return
		}

		v.test.NumberOfParts = s.ObjectParts.PartsCount
		v.test.ETag = tf.UploadInfo.ETag
		v.test.ObjectSize = int(tf.UploadInfo.Size)

		err = validateObjectAttributeRequest(s, &v.opts, &v.test)
		if err != nil {
			logError(testName, function, args, startTime, "", "Validating GetObjectsAttributes response failed, table test: "+i, err)
			return
		}

	}

	logSuccess(testName, function, args, startTime)
}

func testGetObjectAttributesSSECEncryption() {
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObjectAttributes(ctx, bucketName, objectName, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.ObjectAttributesOptions{}",
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{TrailingHeaders: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName
	err = c.MakeBucket(
		context.Background(),
		bucketName,
		minio.MakeBucketOptions{Region: "us-east-1"},
	)
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	objectName := "encrypted-object"
	args["objectName"] = objectName
	bufSize := dataFileMap["datafile-11-MB"]
	reader := getDataReader("datafile-11-MB")

	sse := encrypt.DefaultPBKDF([]byte("word1 word2 word3 word4"), []byte(bucketName+objectName))

	info, err := c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{
		ContentType:          "content/custom",
		SendContentMd5:       false,
		ServerSideEncryption: sse,
		PartSize:             uint64(bufSize) / 2,
		Checksum:             minio.ChecksumCRC32C,
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	opts := minio.ObjectAttributesOptions{
		ServerSideEncryption: sse,
	}
	attr, err := c.GetObjectAttributes(context.Background(), bucketName, objectName, opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObjectAttributes with empty bucket name should have failed", nil)
		return
	}
	err = validateObjectAttributeRequest(attr, &opts, &objectAttributesTestOptions{
		TestFileName:     info.Key,
		ETag:             info.ETag,
		NumberOfParts:    2,
		ObjectSize:       int(info.Size),
		HasFullChecksum:  true,
		HasParts:         true,
		HasPartChecksums: true,
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "Validating GetObjectsAttributes response failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testGetObjectAttributesErrorCases() {
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObjectAttributes(ctx, bucketName, objectName, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.ObjectAttributesOptions{}",
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{TrailingHeaders: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	unknownBucket := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-bucket-")
	unknownObject := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-object-")

	_, err = c.GetObjectAttributes(context.Background(), unknownBucket, unknownObject, minio.ObjectAttributesOptions{})
	if err == nil {
		logError(testName, function, args, startTime, "", "GetObjectAttributes failed", nil)
		return
	}

	errorResponse := err.(minio.ErrorResponse)
	if errorResponse.Code != minio.NoSuchBucket {
		logError(testName, function, args, startTime, "", "Invalid error code, expected NoSuchBucket but got "+errorResponse.Code, nil)
		return
	}

	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName
	err = c.MakeBucket(
		context.Background(),
		bucketName,
		minio.MakeBucketOptions{Region: "us-east-1"},
	)
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	bucketNameV := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-versioned-")
	args["bucketName"] = bucketNameV
	err = c.MakeBucket(
		context.Background(),
		bucketNameV,
		minio.MakeBucketOptions{Region: "us-east-1"},
	)
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}
	err = c.EnableVersioning(context.Background(), bucketNameV)
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, c)
	defer cleanupVersionedBucket(bucketNameV, c)

	_, err = c.GetObjectAttributes(context.Background(), bucketName, unknownObject, minio.ObjectAttributesOptions{})
	if err == nil {
		logError(testName, function, args, startTime, "", "GetObjectAttributes failed", nil)
		return
	}

	errorResponse = err.(minio.ErrorResponse)
	if errorResponse.Code != minio.NoSuchKey {
		logError(testName, function, args, startTime, "", "Invalid error code, expected "+minio.NoSuchKey+" but got "+errorResponse.Code, nil)
		return
	}

	_, err = c.GetObjectAttributes(context.Background(), bucketName, "", minio.ObjectAttributesOptions{})
	if err == nil {
		logError(testName, function, args, startTime, "", "GetObjectAttributes with empty object name should have failed", nil)
		return
	}

	_, err = c.GetObjectAttributes(context.Background(), "", unknownObject, minio.ObjectAttributesOptions{})
	if err == nil {
		logError(testName, function, args, startTime, "", "GetObjectAttributes with empty bucket name should have failed", nil)
		return
	}

	_, err = c.GetObjectAttributes(context.Background(), bucketNameV, unknownObject, minio.ObjectAttributesOptions{
		VersionID: uuid.NewString(),
	})
	if err == nil {
		logError(testName, function, args, startTime, "", "GetObjectAttributes with empty bucket name should have failed", nil)
		return
	}
	errorResponse = err.(minio.ErrorResponse)
	if errorResponse.Code != minio.NoSuchVersion {
		logError(testName, function, args, startTime, "", "Invalid error code, expected "+minio.NoSuchVersion+" but got "+errorResponse.Code, nil)
		return
	}

	logSuccess(testName, function, args, startTime)
}

type objectAttributesNewObject struct {
	Object           string
	ObjectReaderType string
	Bucket           string
	ContentType      string
	SendContentMd5   bool
	UploadInfo       minio.UploadInfo
}

type objectAttributesTableTest struct {
	opts minio.ObjectAttributesOptions
	test objectAttributesTestOptions
}

type objectAttributesTestOptions struct {
	TestFileName     string
	ETag             string
	NumberOfParts    int
	StorageClass     string
	ObjectSize       int
	HasPartChecksums bool
	HasFullChecksum  bool
	HasParts         bool
}

func validateObjectAttributeRequest(OA *minio.ObjectAttributes, opts *minio.ObjectAttributesOptions, test *objectAttributesTestOptions) (err error) {
	if opts.VersionID != "" {
		if OA.VersionID != opts.VersionID {
			err = fmt.Errorf("Expected versionId %s but got versionId %s", opts.VersionID, OA.VersionID)
			return
		}
	}

	partsMissingChecksum := false
	foundPartChecksum := false
	for _, v := range OA.ObjectParts.Parts {
		checksumFound := false
		if v.ChecksumSHA256 != "" {
			checksumFound = true
		} else if v.ChecksumSHA1 != "" {
			checksumFound = true
		} else if v.ChecksumCRC32 != "" {
			checksumFound = true
		} else if v.ChecksumCRC32C != "" {
			checksumFound = true
		}
		if !checksumFound {
			partsMissingChecksum = true
		} else {
			foundPartChecksum = true
		}
	}

	if test.HasPartChecksums {
		if partsMissingChecksum {
			err = fmt.Errorf("One or all parts were missing a checksum")
			return
		}
	} else {
		if foundPartChecksum {
			err = fmt.Errorf("Did not expect ObjectParts to have checksums but found one")
			return
		}
	}

	hasFullObjectChecksum := (OA.Checksum.ChecksumCRC32 != "" ||
		OA.Checksum.ChecksumCRC32C != "" ||
		OA.Checksum.ChecksumSHA1 != "" ||
		OA.Checksum.ChecksumSHA256 != "")

	if test.HasFullChecksum {
		if !hasFullObjectChecksum {
			err = fmt.Errorf("Full object checksum not found")
			return
		}
	} else {
		if hasFullObjectChecksum {
			err = fmt.Errorf("Did not expect a full object checksum but we got one")
			return
		}
	}

	if OA.ETag != test.ETag {
		err = fmt.Errorf("Etags do not match, got %s but expected %s", OA.ETag, test.ETag)
		return
	}

	if test.HasParts {
		if len(OA.ObjectParts.Parts) < 1 {
			err = fmt.Errorf("Was expecting ObjectParts but none were present")
			return
		}
	}

	if OA.StorageClass == "" {
		err = fmt.Errorf("Was expecting a StorageClass but got none")
		return
	}

	if OA.ObjectSize != test.ObjectSize {
		err = fmt.Errorf("Was expecting a ObjectSize but got none")
		return
	}

	if test.HasParts {
		if opts.MaxParts == 0 {
			if len(OA.ObjectParts.Parts) != OA.ObjectParts.PartsCount {
				err = fmt.Errorf("expected %s parts but got %d", OA.ObjectParts.PartsCount, len(OA.ObjectParts.Parts))
				return
			}
		} else if (opts.MaxParts + opts.PartNumberMarker) > OA.ObjectParts.PartsCount {
			if len(OA.ObjectParts.Parts) != (OA.ObjectParts.PartsCount - opts.PartNumberMarker) {
				err = fmt.Errorf("expected %d parts but got %d", (OA.ObjectParts.PartsCount - opts.PartNumberMarker), len(OA.ObjectParts.Parts))
				return
			}
		} else if opts.MaxParts != 0 {
			if opts.MaxParts != len(OA.ObjectParts.Parts) {
				err = fmt.Errorf("expected %d parts but got %d", opts.MaxParts, len(OA.ObjectParts.Parts))
				return
			}
		}
	}

	if OA.ObjectParts.NextPartNumberMarker == OA.ObjectParts.PartsCount {
		if OA.ObjectParts.IsTruncated {
			err = fmt.Errorf("Expected ObjectParts to NOT be truncated, but it was")
			return
		}
	}

	if OA.ObjectParts.NextPartNumberMarker != OA.ObjectParts.PartsCount {
		if !OA.ObjectParts.IsTruncated {
			err = fmt.Errorf("Expected ObjectParts to be truncated, but it was NOT")
			return
		}
	}

	return
}

// Test PutObject using a large data to trigger multipart readat
func testPutObjectWithMetadata() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader,size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.PutObjectOptions{UserMetadata: metadata, Progress: progress}",
	}

	if !isFullMode() {
		logIgnored(testName, function, args, startTime, "Skipping functional tests for short/quick runs")
		return
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Make bucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	// Object custom metadata
	customContentType := "custom/contenttype"

	args["metadata"] = map[string][]string{
		"Content-Type":         {customContentType},
		"X-Amz-Meta-CustomKey": {"extra  spaces  in   value"},
	}

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{
		ContentType: customContentType,
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes returned by PutObject does not match GetObject, expected "+string(bufSize)+" got "+string(st.Size), err)
		return
	}
	if st.ContentType != customContentType && st.ContentType != "application/octet-stream" {
		logError(testName, function, args, startTime, "", "ContentType does not match, expected "+customContentType+" got "+st.ContentType, err)
		return
	}
	if err := crcMatchesName(r, "datafile-129-MB"); err != nil {
		logError(testName, function, args, startTime, "", "data CRC check failed", err)
		return
	}
	if err := r.Close(); err != nil {
		logError(testName, function, args, startTime, "", "Object Close failed", err)
		return
	}
	if err := r.Close(); err == nil {
		logError(testName, function, args, startTime, "", "Object already closed, should respond with error", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testPutObjectWithContentLanguage() {
	// initialize logging params
	objectName := "test-object"
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader, size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": objectName,
		"size":       -1,
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName
	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	data := []byte{}
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(data), int64(0), minio.PutObjectOptions{
		ContentLanguage: "en",
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	objInfo, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}

	if objInfo.Metadata.Get("Content-Language") != "en" {
		logError(testName, function, args, startTime, "", "Expected content-language 'en' doesn't match with StatObject return value", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test put object with streaming signature.
func testPutObjectStreaming() {
	// initialize logging params
	objectName := "test-object"
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader,size,opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": objectName,
		"size":       -1,
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName
	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Upload an object.
	sizes := []int64{0, 64*1024 - 1, 64 * 1024}

	for _, size := range sizes {
		data := newRandomReader(size, size)
		ui, err := c.PutObject(context.Background(), bucketName, objectName, data, int64(size), minio.PutObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObjectStreaming failed", err)
			return
		}

		if ui.Size != size {
			logError(testName, function, args, startTime, "", "PutObjectStreaming result has unexpected size", nil)
			return
		}

		objInfo, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "StatObject failed", err)
			return
		}
		if objInfo.Size != size {
			logError(testName, function, args, startTime, "", "Unexpected size", err)
			return
		}

	}

	logSuccess(testName, function, args, startTime)
}

// Test get object seeker from the end, using whence set to '2'.
func testGetObjectSeekEnd() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes read does not match, expected "+string(int64(bufSize))+" got "+string(st.Size), err)
		return
	}

	pos, err := r.Seek(-100, 2)
	if err != nil {
		logError(testName, function, args, startTime, "", "Object Seek failed", err)
		return
	}
	if pos != st.Size-100 {
		logError(testName, function, args, startTime, "", "Incorrect position", err)
		return
	}
	buf2 := make([]byte, 100)
	m, err := readFull(r, buf2)
	if err != nil {
		logError(testName, function, args, startTime, "", "Error reading through readFull", err)
		return
	}
	if m != len(buf2) {
		logError(testName, function, args, startTime, "", "Number of bytes dont match, expected "+string(len(buf2))+" got "+string(m), err)
		return
	}
	hexBuf1 := fmt.Sprintf("%02x", buf[len(buf)-100:])
	hexBuf2 := fmt.Sprintf("%02x", buf2[:m])
	if hexBuf1 != hexBuf2 {
		logError(testName, function, args, startTime, "", "Values at same index dont match", err)
		return
	}
	pos, err = r.Seek(-100, 2)
	if err != nil {
		logError(testName, function, args, startTime, "", "Object Seek failed", err)
		return
	}
	if pos != st.Size-100 {
		logError(testName, function, args, startTime, "", "Incorrect position", err)
		return
	}
	if err = r.Close(); err != nil {
		logError(testName, function, args, startTime, "", "ObjectClose failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test get object reader to not throw error on being closed twice.
func testGetObjectClosedTwice() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes in stat does not match, expected "+string(int64(bufSize))+" got "+string(st.Size), err)
		return
	}
	if err := crcMatchesName(r, "datafile-33-kB"); err != nil {
		logError(testName, function, args, startTime, "", "data CRC check failed", err)
		return
	}
	if err := r.Close(); err != nil {
		logError(testName, function, args, startTime, "", "Object Close failed", err)
		return
	}
	if err := r.Close(); err == nil {
		logError(testName, function, args, startTime, "", "Already closed object. No error returned", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test RemoveObjects request where context cancels after timeout
func testRemoveObjectsContext() {
	// Initialize logging params.
	startTime := time.Now()
	testName := getFuncName()
	function := "RemoveObjects(ctx, bucketName, objectsCh)"
	args := map[string]interface{}{
		"bucketName": "",
	}

	// Instantiate new minio client.
	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate put data.
	r := bytes.NewReader(bytes.Repeat([]byte("a"), 8))

	// Multi remove of 20 objects.
	nrObjects := 20
	objectsCh := make(chan minio.ObjectInfo)
	go func() {
		defer close(objectsCh)
		for i := 0; i < nrObjects; i++ {
			objectName := "sample" + strconv.Itoa(i) + ".txt"
			info, err := c.PutObject(context.Background(), bucketName, objectName, r, 8,
				minio.PutObjectOptions{ContentType: "application/octet-stream"})
			if err != nil {
				logError(testName, function, args, startTime, "", "PutObject failed", err)
				continue
			}
			objectsCh <- minio.ObjectInfo{
				Key:       info.Key,
				VersionID: info.VersionID,
			}
		}
	}()
	// Set context to cancel in 1 nanosecond.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	args["ctx"] = ctx
	defer cancel()

	// Call RemoveObjects API with short timeout.
	errorCh := c.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})
	// Check for error.
	select {
	case r := <-errorCh:
		if r.Err == nil {
			logError(testName, function, args, startTime, "", "RemoveObjects should fail on short timeout", err)
			return
		}
	}
	// Set context with longer timeout.
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	args["ctx"] = ctx
	defer cancel()
	// Perform RemoveObjects with the longer timeout. Expect the removals to succeed.
	errorCh = c.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})
	select {
	case r, more := <-errorCh:
		if more || r.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error", r.Err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Test removing multiple objects with Remove API
func testRemoveMultipleObjects() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "RemoveObjects(bucketName, objectsCh)"
	args := map[string]interface{}{
		"bucketName": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	r := bytes.NewReader(bytes.Repeat([]byte("a"), 1))

	// Multi remove of 1100 objects
	nrObjects := 1100

	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)
		// Upload objects and send them to objectsCh
		for i := 0; i < nrObjects; i++ {
			objectName := "sample" + strconv.Itoa(i) + ".txt"
			info, err := c.PutObject(context.Background(), bucketName, objectName, r, 1,
				minio.PutObjectOptions{ContentType: "application/octet-stream"})
			if err != nil {
				logError(testName, function, args, startTime, "", "PutObject failed", err)
				continue
			}
			objectsCh <- minio.ObjectInfo{
				Key:       info.Key,
				VersionID: info.VersionID,
			}
		}
	}()

	// Call RemoveObjects API
	errorCh := c.RemoveObjects(context.Background(), bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Check if errorCh doesn't receive any error
	select {
	case r, more := <-errorCh:
		if more {
			logError(testName, function, args, startTime, "", "Unexpected error", r.Err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Test removing multiple objects with Remove API as iterator
func testRemoveMultipleObjectsIter() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "RemoveObjects(bucketName, objectsCh)"
	args := map[string]interface{}{
		"bucketName": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	buf := []byte("a")

	// Multi remove of 1100 objects
	nrObjects := 1100

	objectsIter := func() iter.Seq[minio.ObjectInfo] {
		return func(yield func(minio.ObjectInfo) bool) {
			// Upload objects and send them to objectsCh
			for i := 0; i < nrObjects; i++ {
				objectName := "sample" + strconv.Itoa(i) + ".txt"
				info, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), 1,
					minio.PutObjectOptions{ContentType: "application/octet-stream"})
				if err != nil {
					logError(testName, function, args, startTime, "", "PutObject failed", err)
					continue
				}
				if !yield(minio.ObjectInfo{
					Key:       info.Key,
					VersionID: info.VersionID,
				}) {
					return
				}
			}
		}
	}

	// Call RemoveObjects API
	results, err := c.RemoveObjectsWithIter(context.Background(), bucketName, objectsIter(), minio.RemoveObjectsOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Unexpected error", err)
		return
	}

	for result := range results {
		if result.Err != nil {
			logError(testName, function, args, startTime, "", "Unexpected error", result.Err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Test removing multiple objects and check for results
func testRemoveMultipleObjectsWithResult() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "RemoveObjects(bucketName, objectsCh)"
	args := map[string]interface{}{
		"bucketName": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupVersionedBucket(bucketName, c)

	buf := []byte("a")

	nrObjects := 10
	nrLockedObjects := 5

	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)
		// Upload objects and send them to objectsCh
		for i := 0; i < nrObjects; i++ {
			objectName := "sample" + strconv.Itoa(i) + ".txt"
			info, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), 1,
				minio.PutObjectOptions{ContentType: "application/octet-stream"})
			if err != nil {
				logError(testName, function, args, startTime, "", "PutObject failed", err)
				return
			}
			if i < nrLockedObjects {
				// t := time.Date(2130, time.April, 25, 14, 0, 0, 0, time.UTC)
				t := time.Now().Add(5 * time.Minute)
				m := minio.RetentionMode(minio.Governance)
				opts := minio.PutObjectRetentionOptions{
					GovernanceBypass: false,
					RetainUntilDate:  &t,
					Mode:             &m,
					VersionID:        info.VersionID,
				}
				err = c.PutObjectRetention(context.Background(), bucketName, objectName, opts)
				if err != nil {
					logError(testName, function, args, startTime, "", "Error setting retention", err)
					return
				}
			}

			objectsCh <- minio.ObjectInfo{
				Key:       info.Key,
				VersionID: info.VersionID,
			}
		}
	}()

	// Call RemoveObjects API
	resultCh := c.RemoveObjectsWithResult(context.Background(), bucketName, objectsCh, minio.RemoveObjectsOptions{})

	var foundNil, foundErr int

	for {
		// Check if errorCh doesn't receive any error
		select {
		case deleteRes, ok := <-resultCh:
			if !ok {
				goto out
			}
			if deleteRes.ObjectName == "" {
				logError(testName, function, args, startTime, "", "Unexpected object name", nil)
				return
			}
			if deleteRes.ObjectVersionID == "" {
				logError(testName, function, args, startTime, "", "Unexpected object version ID", nil)
				return
			}

			if deleteRes.Err == nil {
				foundNil++
			} else {
				foundErr++
			}
		}
	}
out:
	if foundNil+foundErr != nrObjects {
		logError(testName, function, args, startTime, "", "Unexpected number of results", nil)
		return
	}

	if foundNil != nrObjects-nrLockedObjects {
		logError(testName, function, args, startTime, "", "Unexpected number of nil errors", nil)
		return
	}

	if foundErr != nrLockedObjects {
		logError(testName, function, args, startTime, "", "Unexpected number of errors", nil)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests FPutObject of a big file to trigger multipart
func testFPutObjectMultipart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FPutObject(bucketName, objectName, fileName, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"fileName":   "",
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Upload 4 parts to utilize all 3 'workers' in multipart and still have a part to upload.
	fileName := getMintDataDirFilePath("datafile-129-MB")
	if fileName == "" {
		// Make a temp file with minPartSize bytes of data.
		file, err := os.CreateTemp(os.TempDir(), "FPutObjectTest")
		if err != nil {
			logError(testName, function, args, startTime, "", "TempFile creation failed", err)
			return
		}
		// Upload 2 parts to utilize all 3 'workers' in multipart and still have a part to upload.
		if _, err = io.Copy(file, getDataReader("datafile-129-MB")); err != nil {
			logError(testName, function, args, startTime, "", "Copy failed", err)
			return
		}
		if err = file.Close(); err != nil {
			logError(testName, function, args, startTime, "", "File Close failed", err)
			return
		}
		fileName = file.Name()
		args["fileName"] = fileName
	}
	totalSize := dataFileMap["datafile-129-MB"]
	// Set base object name
	objectName := bucketName + "FPutObject" + "-standard"
	args["objectName"] = objectName

	objectContentType := "testapplication/octet-stream"
	args["objectContentType"] = objectContentType

	// Perform standard FPutObject with contentType provided (Expecting application/octet-stream)
	_, err = c.FPutObject(context.Background(), bucketName, objectName, fileName, minio.PutObjectOptions{ContentType: objectContentType})
	if err != nil {
		logError(testName, function, args, startTime, "", "FPutObject failed", err)
		return
	}

	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	objInfo, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Unexpected error", err)
		return
	}
	if objInfo.Size != int64(totalSize) {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(int64(totalSize))+" got "+string(objInfo.Size), err)
		return
	}
	if objInfo.ContentType != objectContentType && objInfo.ContentType != "application/octet-stream" {
		logError(testName, function, args, startTime, "", "ContentType doesn't match", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests FPutObject with null contentType (default = application/octet-stream)
func testFPutObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FPutObject(bucketName, objectName, fileName, opts)"

	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"fileName":   "",
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	location := "us-east-1"

	// Make a new bucket.
	args["bucketName"] = bucketName
	args["location"] = location
	function = "MakeBucket(bucketName, location)"
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: location})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Upload 3 parts worth of data to use all 3 of multiparts 'workers' and have an extra part.
	// Use different data in part for multipart tests to check parts are uploaded in correct order.
	fName := getMintDataDirFilePath("datafile-129-MB")
	if fName == "" {
		// Make a temp file with minPartSize bytes of data.
		file, err := os.CreateTemp(os.TempDir(), "FPutObjectTest")
		if err != nil {
			logError(testName, function, args, startTime, "", "TempFile creation failed", err)
			return
		}

		// Upload 3 parts to utilize all 3 'workers' in multipart and still have a part to upload.
		if _, err = io.Copy(file, getDataReader("datafile-129-MB")); err != nil {
			logError(testName, function, args, startTime, "", "File copy failed", err)
			return
		}
		// Close the file pro-actively for windows.
		if err = file.Close(); err != nil {
			logError(testName, function, args, startTime, "", "File close failed", err)
			return
		}
		defer os.Remove(file.Name())
		fName = file.Name()
	}

	// Set base object name
	function = "FPutObject(bucketName, objectName, fileName, opts)"
	objectName := bucketName + "FPutObject"
	args["objectName"] = objectName + "-standard"
	args["fileName"] = fName
	args["opts"] = minio.PutObjectOptions{ContentType: "application/octet-stream"}

	// Perform standard FPutObject with contentType provided (Expecting application/octet-stream)
	ui, err := c.FPutObject(context.Background(), bucketName, objectName+"-standard", fName, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "FPutObject failed", err)
		return
	}

	if ui.Size != int64(dataFileMap["datafile-129-MB"]) {
		logError(testName, function, args, startTime, "", "FPutObject returned an unexpected upload size", err)
		return
	}

	// Perform FPutObject with no contentType provided (Expecting application/octet-stream)
	args["objectName"] = objectName + "-Octet"
	_, err = c.FPutObject(context.Background(), bucketName, objectName+"-Octet", fName, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "File close failed", err)
		return
	}

	srcFile, err := os.Open(fName)
	if err != nil {
		logError(testName, function, args, startTime, "", "File open failed", err)
		return
	}
	defer srcFile.Close()
	// Add extension to temp file name
	tmpFile, err := os.Create(fName + ".gtar")
	if err != nil {
		logError(testName, function, args, startTime, "", "File create failed", err)
		return
	}
	_, err = io.Copy(tmpFile, srcFile)
	if err != nil {
		logError(testName, function, args, startTime, "", "File copy failed", err)
		return
	}
	tmpFile.Close()

	// Perform FPutObject with no contentType provided (Expecting application/x-gtar)
	args["objectName"] = objectName + "-GTar"
	args["opts"] = minio.PutObjectOptions{}
	_, err = c.FPutObject(context.Background(), bucketName, objectName+"-GTar", fName+".gtar", minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FPutObject failed", err)
		return
	}

	// Check headers
	function = "StatObject(bucketName, objectName, opts)"
	args["objectName"] = objectName + "-standard"
	rStandard, err := c.StatObject(context.Background(), bucketName, objectName+"-standard", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}
	if rStandard.ContentType != "application/octet-stream" {
		logError(testName, function, args, startTime, "", "ContentType does not match, expected application/octet-stream, got "+rStandard.ContentType, err)
		return
	}

	function = "StatObject(bucketName, objectName, opts)"
	args["objectName"] = objectName + "-Octet"
	rOctet, err := c.StatObject(context.Background(), bucketName, objectName+"-Octet", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}
	if rOctet.ContentType != "application/octet-stream" {
		logError(testName, function, args, startTime, "", "ContentType does not match, expected application/octet-stream, got "+rOctet.ContentType, err)
		return
	}

	function = "StatObject(bucketName, objectName, opts)"
	args["objectName"] = objectName + "-GTar"
	rGTar, err := c.StatObject(context.Background(), bucketName, objectName+"-GTar", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}
	if rGTar.ContentType != "application/x-gtar" && rGTar.ContentType != "application/octet-stream" && rGTar.ContentType != "application/x-tar" {
		logError(testName, function, args, startTime, "", "ContentType does not match, expected application/x-tar or application/octet-stream, got "+rGTar.ContentType, err)
		return
	}

	os.Remove(fName + ".gtar")
	logSuccess(testName, function, args, startTime)
}

// Tests FPutObject request when context cancels after timeout
func testFPutObjectContext() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FPutObject(bucketName, objectName, fileName, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"fileName":   "",
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Upload 1 parts worth of data to use multipart upload.
	// Use different data in part for multipart tests to check parts are uploaded in correct order.
	fName := getMintDataDirFilePath("datafile-1-MB")
	if fName == "" {
		// Make a temp file with 1 MiB bytes of data.
		file, err := os.CreateTemp(os.TempDir(), "FPutObjectContextTest")
		if err != nil {
			logError(testName, function, args, startTime, "", "TempFile creation failed", err)
			return
		}

		// Upload 1 parts to trigger multipart upload
		if _, err = io.Copy(file, getDataReader("datafile-1-MB")); err != nil {
			logError(testName, function, args, startTime, "", "File copy failed", err)
			return
		}
		// Close the file pro-actively for windows.
		if err = file.Close(); err != nil {
			logError(testName, function, args, startTime, "", "File close failed", err)
			return
		}
		defer os.Remove(file.Name())
		fName = file.Name()
	}

	// Set base object name
	objectName := bucketName + "FPutObjectContext"
	args["objectName"] = objectName
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	args["ctx"] = ctx
	defer cancel()

	// Perform FPutObject with contentType provided (Expecting application/octet-stream)
	_, err = c.FPutObject(ctx, bucketName, objectName+"-Shorttimeout", fName, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err == nil {
		logError(testName, function, args, startTime, "", "FPutObject should fail on short timeout", err)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()
	// Perform FPutObject with a long timeout. Expect the put object to succeed
	_, err = c.FPutObject(ctx, bucketName, objectName+"-Longtimeout", fName, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FPutObject shouldn't fail on long timeout", err)
		return
	}

	_, err = c.StatObject(context.Background(), bucketName, objectName+"-Longtimeout", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests FPutObject request when context cancels after timeout
func testFPutObjectContextV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FPutObjectContext(ctx, bucketName, objectName, fileName, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"opts":       "minio.PutObjectOptions{ContentType:objectContentType}",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Upload 1 parts worth of data to use multipart upload.
	// Use different data in part for multipart tests to check parts are uploaded in correct order.
	fName := getMintDataDirFilePath("datafile-1-MB")
	if fName == "" {
		// Make a temp file with 1 MiB bytes of data.
		file, err := os.CreateTemp(os.TempDir(), "FPutObjectContextTest")
		if err != nil {
			logError(testName, function, args, startTime, "", "Temp file creation failed", err)
			return
		}

		// Upload 1 parts to trigger multipart upload
		if _, err = io.Copy(file, getDataReader("datafile-1-MB")); err != nil {
			logError(testName, function, args, startTime, "", "File copy failed", err)
			return
		}

		// Close the file pro-actively for windows.
		if err = file.Close(); err != nil {
			logError(testName, function, args, startTime, "", "File close failed", err)
			return
		}
		defer os.Remove(file.Name())
		fName = file.Name()
	}

	// Set base object name
	objectName := bucketName + "FPutObjectContext"
	args["objectName"] = objectName

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	args["ctx"] = ctx
	defer cancel()

	// Perform FPutObject with contentType provided (Expecting application/octet-stream)
	_, err = c.FPutObject(ctx, bucketName, objectName+"-Shorttimeout", fName, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err == nil {
		logError(testName, function, args, startTime, "", "FPutObject should fail on short timeout", err)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()
	// Perform FPutObject with a long timeout. Expect the put object to succeed
	_, err = c.FPutObject(ctx, bucketName, objectName+"-Longtimeout", fName, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FPutObject shouldn't fail on longer timeout", err)
		return
	}

	_, err = c.StatObject(context.Background(), bucketName, objectName+"-Longtimeout", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test validates putObject with context to see if request cancellation is honored.
func testPutObjectContext() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(ctx, bucketName, objectName, fileName, opts)"
	args := map[string]interface{}{
		"ctx":        "",
		"bucketName": "",
		"objectName": "",
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Make a new bucket.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket call failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()
	objectName := fmt.Sprintf("test-file-%v", rand.Uint32())
	args["objectName"] = objectName

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	cancel()
	args["ctx"] = ctx
	args["opts"] = minio.PutObjectOptions{ContentType: "binary/octet-stream"}

	_, err = c.PutObject(ctx, bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err == nil {
		logError(testName, function, args, startTime, "", "PutObject should fail on short timeout", err)
		return
	}

	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	args["ctx"] = ctx

	defer cancel()
	reader = getDataReader("datafile-33-kB")
	defer reader.Close()
	_, err = c.PutObject(ctx, bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject with long timeout failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests get object with s3zip extensions.
func testGetObjectS3Zip() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{"x-minio-extract": true}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer func() {
		// Delete all objects and buckets
		if err = cleanupBucket(bucketName, c); err != nil {
			logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
			return
		}
	}()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "") + ".zip"
	args["objectName"] = objectName

	var zipFile bytes.Buffer
	zw := zip.NewWriter(&zipFile)
	rng := rand.New(rand.NewSource(0xc0cac01a))
	const nFiles = 500
	for i := 0; i <= nFiles; i++ {
		if i == nFiles {
			// Make one large, compressible file.
			i = 1000000
		}
		b := make([]byte, i)
		if i < nFiles {
			rng.Read(b)
		}
		wc, err := zw.Create(fmt.Sprintf("test/small/file-%d.bin", i))
		if err != nil {
			logError(testName, function, args, startTime, "", "zw.Create failed", err)
			return
		}
		wc.Write(b)
	}
	err = zw.Close()
	if err != nil {
		logError(testName, function, args, startTime, "", "zw.Close failed", err)
		return
	}
	buf := zipFile.Bytes()

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat object failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(len(buf))+", got "+string(st.Size), err)
		return
	}
	r.Close()

	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		logError(testName, function, args, startTime, "", "zip.NewReader failed", err)
		return
	}
	lOpts := minio.ListObjectsOptions{}
	lOpts.Set("x-minio-extract", "true")
	lOpts.Prefix = objectName + "/"
	lOpts.Recursive = true
	list := c.ListObjects(context.Background(), bucketName, lOpts)
	listed := map[string]minio.ObjectInfo{}
	for item := range list {
		if item.Err != nil {
			break
		}
		listed[item.Key] = item
	}
	if len(listed) == 0 {
		// Assume we are running against non-minio.
		args["SKIPPED"] = true
		logIgnored(testName, function, args, startTime, "s3zip does not appear to be present")
		return
	}

	for _, file := range zr.File {
		if file.FileInfo().IsDir() {
			continue
		}
		args["zipfile"] = file.Name
		zfr, err := file.Open()
		if err != nil {
			logError(testName, function, args, startTime, "", "file.Open failed", err)
			return
		}
		want, err := io.ReadAll(zfr)
		if err != nil {
			logError(testName, function, args, startTime, "", "fzip file read failed", err)
			return
		}

		opts := minio.GetObjectOptions{}
		opts.Set("x-minio-extract", "true")
		key := path.Join(objectName, file.Name)
		r, err = c.GetObject(context.Background(), bucketName, key, opts)
		if err != nil {
			terr := minio.ToErrorResponse(err)
			if terr.StatusCode != http.StatusNotFound {
				logError(testName, function, args, startTime, "", "GetObject failed", err)
			}
			return
		}
		got, err := io.ReadAll(r)
		if err != nil {
			logError(testName, function, args, startTime, "", "ReadAll failed", err)
			return
		}
		r.Close()
		if !bytes.Equal(want, got) {
			logError(testName, function, args, startTime, "", "Content mismatch", err)
			return
		}
		oi, ok := listed[key]
		if !ok {
			logError(testName, function, args, startTime, "", "Object Missing", fmt.Errorf("%s not present in listing", key))
			return
		}
		if int(oi.Size) != len(got) {
			logError(testName, function, args, startTime, "", "Object Size Incorrect", fmt.Errorf("listing %d, read %d", oi.Size, len(got)))
			return
		}
		delete(listed, key)
	}
	delete(args, "zipfile")
	if len(listed) > 0 {
		logError(testName, function, args, startTime, "", "Extra listed objects", fmt.Errorf("left over: %v", listed))
		return
	}
	logSuccess(testName, function, args, startTime)
}

// Tests get object ReaderSeeker interface methods.
func testGetObjectReadSeekFunctional() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer func() {
		// Delete all objects and buckets
		if err = cleanupBucket(bucketName, c); err != nil {
			logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
			return
		}
	}()

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat object failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(int64(bufSize))+", got "+string(st.Size), err)
		return
	}

	// This following function helps us to compare data from the reader after seek
	// with the data from the original buffer
	cmpData := func(r io.Reader, start, end int) {
		if end-start == 0 {
			return
		}
		buffer := bytes.NewBuffer([]byte{})
		if _, err := io.CopyN(buffer, r, int64(bufSize)); err != nil {
			if err != io.EOF {
				logError(testName, function, args, startTime, "", "CopyN failed", err)
				return
			}
		}
		if !bytes.Equal(buf[start:end], buffer.Bytes()) {
			logError(testName, function, args, startTime, "", "Incorrect read bytes v/s original buffer", err)
			return
		}
	}

	// Generic seek error for errors other than io.EOF
	seekErr := errors.New("seek error")

	testCases := []struct {
		offset    int64
		whence    int
		pos       int64
		err       error
		shouldCmp bool
		start     int
		end       int
	}{
		// Start from offset 0, fetch data and compare
		{0, 0, 0, nil, true, 0, 0},
		// Start from offset 2048, fetch data and compare
		{2048, 0, 2048, nil, true, 2048, bufSize},
		// Start from offset larger than possible
		{int64(bufSize) + 1024, 0, 0, seekErr, false, 0, 0},
		// Move to offset 0 without comparing
		{0, 0, 0, nil, false, 0, 0},
		// Move one step forward and compare
		{1, 1, 1, nil, true, 1, bufSize},
		// Move larger than possible
		{int64(bufSize), 1, 0, seekErr, false, 0, 0},
		// Provide negative offset with CUR_SEEK
		{int64(-1), 1, 0, seekErr, false, 0, 0},
		// Test with whence SEEK_END and with positive offset
		{1024, 2, int64(bufSize) - 1024, io.EOF, true, 0, 0},
		// Test with whence SEEK_END and with negative offset
		{-1024, 2, int64(bufSize) - 1024, nil, true, bufSize - 1024, bufSize},
		// Test with whence SEEK_END and with large negative offset
		{-int64(bufSize) * 2, 2, 0, seekErr, true, 0, 0},
	}

	for i, testCase := range testCases {
		// Perform seek operation
		n, err := r.Seek(testCase.offset, testCase.whence)
		// We expect an error
		if testCase.err == seekErr && err == nil {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", unexpected err value: expected: "+testCase.err.Error()+", found: "+err.Error(), err)
			return
		}
		// We expect a specific error
		if testCase.err != seekErr && testCase.err != err {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", unexpected err value: expected: "+testCase.err.Error()+", found: "+err.Error(), err)
			return
		}
		// If we expect an error go to the next loop
		if testCase.err != nil {
			continue
		}
		// Check the returned seek pos
		if n != testCase.pos {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", number of bytes seeked does not match, expected "+string(testCase.pos)+", got "+string(n), err)
			return
		}
		// Compare only if shouldCmp is activated
		if testCase.shouldCmp {
			cmpData(r, testCase.start, testCase.end)
		}
	}
	logSuccess(testName, function, args, startTime)
}

// Tests get object ReaderAt interface methods.
func testGetObjectReadAtFunctional() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	offset := int64(2048)

	// read directly
	buf1 := make([]byte, 512)
	buf2 := make([]byte, 512)
	buf3 := make([]byte, 512)
	buf4 := make([]byte, 512)

	// Test readAt before stat is called such that objectInfo doesn't change.
	m, err := r.ReadAt(buf1, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf1) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf1))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf1, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes in stat does not match, expected "+string(int64(bufSize))+", got "+string(st.Size), err)
		return
	}

	m, err = r.ReadAt(buf2, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf2) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf2))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf2, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}

	offset += 512
	m, err = r.ReadAt(buf3, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf3) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf3))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf3, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512
	m, err = r.ReadAt(buf4, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf4) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf4))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf4, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}

	buf5 := make([]byte, len(buf))
	// Read the whole object.
	m, err = r.ReadAt(buf5, 0)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "ReadAt failed", err)
			return
		}
	}
	if m != len(buf5) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf5))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf, buf5) {
		logError(testName, function, args, startTime, "", "Incorrect data read in GetObject, than what was previously uploaded", err)
		return
	}

	buf6 := make([]byte, len(buf)+1)
	// Read the whole object and beyond.
	_, err = r.ReadAt(buf6, 0)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "ReadAt failed", err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Reproduces issue https://github.com/minio/minio-go/issues/1137
func testGetObjectReadAtWhenEOFWasReached() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// read directly
	buf1 := make([]byte, len(buf))
	buf2 := make([]byte, 512)

	m, err := r.Read(buf1)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "Read failed", err)
			return
		}
	}
	if m != len(buf1) {
		logError(testName, function, args, startTime, "", "Read read shorter bytes before reaching EOF, expected "+string(len(buf1))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf1, buf) {
		logError(testName, function, args, startTime, "", "Incorrect count of Read data", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes in stat does not match, expected "+string(int64(bufSize))+", got "+string(st.Size), err)
		return
	}

	m, err = r.ReadAt(buf2, 512)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf2) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf2))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf2, buf[512:1024]) {
		logError(testName, function, args, startTime, "", "Incorrect count of ReadAt data", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test Presigned Post Policy
func testPresignedPostPolicy() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PresignedPostPolicy(policy)"
	args := map[string]interface{}{
		"policy": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	// Azure requires the key to not start with a number
	metadataKey := randString(60, rand.NewSource(time.Now().UnixNano()), "user")
	metadataValue := randString(60, rand.NewSource(time.Now().UnixNano()), "")

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	policy := minio.NewPostPolicy()
	policy.SetBucket(bucketName)
	policy.SetKey(objectName)
	policy.SetExpires(time.Now().UTC().AddDate(0, 0, 10)) // expires in 10 days
	policy.SetContentType("binary/octet-stream")
	policy.SetContentLengthRange(10, 1024*1024)
	policy.SetUserMetadata(metadataKey, metadataValue)
	policy.SetContentEncoding("gzip")

	// Add CRC32C
	checksum := minio.ChecksumCRC32C.ChecksumBytes(buf)
	err = policy.SetChecksum(checksum)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetChecksum failed", err)
		return
	}

	args["policy"] = policy.String()

	presignedPostPolicyURL, formData, err := c.PresignedPostPolicy(context.Background(), policy)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedPostPolicy failed", err)
		return
	}

	var formBuf bytes.Buffer
	writer := multipart.NewWriter(&formBuf)
	for k, v := range formData {
		writer.WriteField(k, v)
	}

	// Get a 33KB file to upload and test if set post policy works
	filePath := getMintDataDirFilePath("datafile-33-kB")
	if filePath == "" {
		// Make a temp file with 33 KB data.
		file, err := os.CreateTemp(os.TempDir(), "PresignedPostPolicyTest")
		if err != nil {
			logError(testName, function, args, startTime, "", "TempFile creation failed", err)
			return
		}
		if _, err = io.Copy(file, getDataReader("datafile-33-kB")); err != nil {
			logError(testName, function, args, startTime, "", "Copy failed", err)
			return
		}
		if err = file.Close(); err != nil {
			logError(testName, function, args, startTime, "", "File Close failed", err)
			return
		}
		filePath = file.Name()
	}

	// add file to post request
	f, err := os.Open(filePath)
	defer f.Close()
	if err != nil {
		logError(testName, function, args, startTime, "", "File open failed", err)
		return
	}
	w, err := writer.CreateFormFile("file", filePath)
	if err != nil {
		logError(testName, function, args, startTime, "", "CreateFormFile failed", err)
		return
	}

	_, err = io.Copy(w, f)
	if err != nil {
		logError(testName, function, args, startTime, "", "Copy failed", err)
		return
	}
	writer.Close()

	httpClient := &http.Client{
		// Setting a sensible time out of 30secs to wait for response
		// headers. Request is pro-actively canceled after 30secs
		// with no response.
		Timeout:   30 * time.Second,
		Transport: createHTTPTransport(),
	}
	args["url"] = presignedPostPolicyURL.String()

	req, err := http.NewRequest(http.MethodPost, presignedPostPolicyURL.String(), bytes.NewReader(formBuf.Bytes()))
	if err != nil {
		logError(testName, function, args, startTime, "", "Http request failed", err)
		return
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	// make post request with correct form data
	res, err := httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "Http request failed", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		logError(testName, function, args, startTime, "", "Http request failed", errors.New(res.Status))
		return
	}

	// expected path should be absolute path of the object
	var scheme string
	if mustParseBool(os.Getenv(enableHTTPS)) {
		scheme = "https://"
	} else {
		scheme = "http://"
	}

	expectedLocation := scheme + os.Getenv(serverEndpoint) + "/" + bucketName + "/" + objectName
	expectedLocationBucketDNS := scheme + bucketName + "." + os.Getenv(serverEndpoint) + "/" + objectName

	if !strings.Contains(expectedLocation, ".amazonaws.com/") {
		// Test when not against AWS S3.
		if val, ok := res.Header["Location"]; ok {
			if val[0] != expectedLocation && val[0] != expectedLocationBucketDNS {
				logError(testName, function, args, startTime, "", fmt.Sprintf("Location in header response is incorrect. Want %q or %q, got %q", expectedLocation, expectedLocationBucketDNS, val[0]), err)
				return
			}
		} else {
			logError(testName, function, args, startTime, "", "Location not found in header response", err)
			return
		}
	}
	wantChecksumCrc32c := checksum.Encoded()
	if got := res.Header.Get("X-Amz-Checksum-Crc32c"); got != wantChecksumCrc32c {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Want checksum %q, got %q", wantChecksumCrc32c, got), nil)
		return
	}

	// Ensure that when we subsequently GetObject, the checksum is returned
	gopts := minio.GetObjectOptions{Checksum: true}
	r, err := c.GetObject(context.Background(), bucketName, objectName, gopts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	if st.ChecksumCRC32C != wantChecksumCrc32c {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Want checksum %s, got %s", wantChecksumCrc32c, st.ChecksumCRC32C), nil)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// testPresignedPostPolicyWrongFile tests that when we have a policy with a checksum, we cannot POST the wrong file
func testPresignedPostPolicyWrongFile() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PresignedPostPolicy(policy)"
	args := map[string]interface{}{
		"policy": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	// Azure requires the key to not start with a number
	metadataKey := randString(60, rand.NewSource(time.Now().UnixNano()), "user")
	metadataValue := randString(60, rand.NewSource(time.Now().UnixNano()), "")

	policy := minio.NewPostPolicy()
	policy.SetBucket(bucketName)
	policy.SetKey(objectName)
	policy.SetExpires(time.Now().UTC().AddDate(0, 0, 10)) // expires in 10 days
	policy.SetContentType("binary/octet-stream")
	policy.SetContentLengthRange(10, 1024*1024)
	policy.SetUserMetadata(metadataKey, metadataValue)

	// Add CRC32C of some data that the policy will explicitly allow.
	checksum := minio.ChecksumCRC32C.ChecksumBytes([]byte{0x01, 0x02, 0x03})
	err = policy.SetChecksum(checksum)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetChecksum failed", err)
		return
	}

	args["policy"] = policy.String()

	presignedPostPolicyURL, formData, err := c.PresignedPostPolicy(context.Background(), policy)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedPostPolicy failed", err)
		return
	}

	// At this stage, we have a policy that allows us to upload for a specific checksum.
	// Test that uploading datafile-10-kB, with a different checksum, fails as expected
	filePath := getMintDataDirFilePath("datafile-10-kB")
	if filePath == "" {
		// Make a temp file with 10 KB data.
		file, err := os.CreateTemp(os.TempDir(), "PresignedPostPolicyTest")
		if err != nil {
			logError(testName, function, args, startTime, "", "TempFile creation failed", err)
			return
		}
		if _, err = io.Copy(file, getDataReader("datafile-10-kB")); err != nil {
			logError(testName, function, args, startTime, "", "Copy failed", err)
			return
		}
		if err = file.Close(); err != nil {
			logError(testName, function, args, startTime, "", "File Close failed", err)
			return
		}
		filePath = file.Name()
	}
	fileReader := getDataReader("datafile-10-kB")
	defer fileReader.Close()
	buf10k, err := io.ReadAll(fileReader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}
	otherChecksum := minio.ChecksumCRC32C.ChecksumBytes(buf10k)

	var formBuf bytes.Buffer
	writer := multipart.NewWriter(&formBuf)
	for k, v := range formData {
		if k == "x-amz-checksum-crc32c" {
			v = otherChecksum.Encoded()
		}
		writer.WriteField(k, v)
	}

	// Add file to post request
	f, err := os.Open(filePath)
	defer f.Close()
	if err != nil {
		logError(testName, function, args, startTime, "", "File open failed", err)
		return
	}
	w, err := writer.CreateFormFile("file", filePath)
	if err != nil {
		logError(testName, function, args, startTime, "", "CreateFormFile failed", err)
		return
	}
	_, err = io.Copy(w, f)
	if err != nil {
		logError(testName, function, args, startTime, "", "Copy failed", err)
		return
	}
	writer.Close()

	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: createHTTPTransport(),
	}
	args["url"] = presignedPostPolicyURL.String()

	req, err := http.NewRequest(http.MethodPost, presignedPostPolicyURL.String(), bytes.NewReader(formBuf.Bytes()))
	if err != nil {
		logError(testName, function, args, startTime, "", "HTTP request failed", err)
		return
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Make the POST request with the form data.
	res, err := httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "HTTP request failed", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		logError(testName, function, args, startTime, "", "HTTP request unexpected status", errors.New(res.Status))
		return
	}

	// Read the response body, ensure it has checksum failure message
	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Normalize the response body, because S3 uses quotes around the policy condition components
	// in the error message, MinIO does not.
	resBodyStr := strings.ReplaceAll(string(resBody), `"`, "")
	if !strings.Contains(resBodyStr, "Policy Condition failed: [eq, $x-amz-checksum-crc32c, 8TDyHg=") {
		logError(testName, function, args, startTime, "", "Unexpected response body", errors.New(resBodyStr))
		return
	}

	logSuccess(testName, function, args, startTime)
}

// testPresignedPostPolicyEmptyFileName tests that an empty file name in the presigned post policy
func testPresignedPostPolicyEmptyFileName() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PresignedPostPolicy(policy)"
	args := map[string]interface{}{
		"policy": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	// Azure requires the key to not start with a number
	metadataKey := randString(60, rand.NewSource(time.Now().UnixNano()), "user")
	metadataValue := randString(60, rand.NewSource(time.Now().UnixNano()), "")

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	policy := minio.NewPostPolicy()
	policy.SetBucket(bucketName)
	policy.SetKey(objectName)
	policy.SetExpires(time.Now().UTC().AddDate(0, 0, 10)) // expires in 10 days
	policy.SetContentType("binary/octet-stream")
	policy.SetContentLengthRange(10, 1024*1024)
	policy.SetUserMetadata(metadataKey, metadataValue)
	policy.SetContentEncoding("gzip")

	// Add CRC32C
	checksum := minio.ChecksumCRC32C.ChecksumBytes(buf)
	err = policy.SetChecksum(checksum)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetChecksum failed", err)
		return
	}

	args["policy"] = policy.String()

	presignedPostPolicyURL, formData, err := c.PresignedPostPolicy(context.Background(), policy)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedPostPolicy failed", err)
		return
	}

	var formBuf bytes.Buffer
	writer := multipart.NewWriter(&formBuf)
	for k, v := range formData {
		writer.WriteField(k, v)
	}

	// Get a 33KB file to upload and test if set post policy works
	filePath := getMintDataDirFilePath("datafile-33-kB")
	if filePath == "" {
		// Make a temp file with 33 KB data.
		file, err := os.CreateTemp(os.TempDir(), "PresignedPostPolicyTest")
		if err != nil {
			logError(testName, function, args, startTime, "", "TempFile creation failed", err)
			return
		}
		if _, err = io.Copy(file, getDataReader("datafile-33-kB")); err != nil {
			logError(testName, function, args, startTime, "", "Copy failed", err)
			return
		}
		if err = file.Close(); err != nil {
			logError(testName, function, args, startTime, "", "File Close failed", err)
			return
		}
		filePath = file.Name()
	}

	// add file to post request
	f, err := os.Open(filePath)
	defer f.Close()
	if err != nil {
		logError(testName, function, args, startTime, "", "File open failed", err)
		return
	}
	w, err := writer.CreateFormFile("", filePath)
	if err != nil {
		logError(testName, function, args, startTime, "", "CreateFormFile failed", err)
		return
	}

	_, err = io.Copy(w, f)
	if err != nil {
		logError(testName, function, args, startTime, "", "Copy failed", err)
		return
	}
	writer.Close()

	httpClient := &http.Client{
		// Setting a sensible time out of 30secs to wait for response
		// headers. Request is pro-actively canceled after 30secs
		// with no response.
		Timeout:   30 * time.Second,
		Transport: createHTTPTransport(),
	}
	args["url"] = presignedPostPolicyURL.String()

	req, err := http.NewRequest(http.MethodPost, presignedPostPolicyURL.String(), bytes.NewReader(formBuf.Bytes()))
	if err != nil {
		logError(testName, function, args, startTime, "", "Http request failed", err)
		return
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	// make post request with correct form data
	res, err := httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "Http request failed", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		logError(testName, function, args, startTime, "", "Http request failed", errors.New(res.Status))
		return
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}
	if !strings.Contains(string(body), "MalformedPOSTRequest") {
		logError(testName, function, args, startTime, "", "Invalid error from server", errors.New(string(body)))
	}

	logSuccess(testName, function, args, startTime)
}

// Tests copy object
func testCopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(dst, src)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Make a new bucket in 'us-east-1' (destination bucket).
	err = c.MakeBucket(context.Background(), bucketName+"-copy", minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName+"-copy", c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	// Check the various fields of source object against destination object.
	objInfo, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	// Copy Source
	src := minio.CopySrcOptions{
		Bucket: bucketName,
		Object: objectName,
		// Set copy conditions.
		MatchETag:          objInfo.ETag,
		MatchModifiedSince: time.Date(2014, time.April, 0, 0, 0, 0, 0, time.UTC),
	}
	args["src"] = src

	dst := minio.CopyDestOptions{
		Bucket: bucketName + "-copy",
		Object: objectName + "-copy",
	}

	// Perform the Copy
	if _, err = c.CopyObject(context.Background(), dst, src); err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed", err)
		return
	}

	// Source object
	r, err = c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	// Destination object
	readerCopy, err := c.GetObject(context.Background(), bucketName+"-copy", objectName+"-copy", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	// Check the various fields of source object against destination object.
	objInfo, err = r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	objInfoCopy, err := readerCopy.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	if objInfo.Size != objInfoCopy.Size {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(objInfoCopy.Size)+", got "+string(objInfo.Size), err)
		return
	}

	if err := crcMatchesName(r, "datafile-33-kB"); err != nil {
		logError(testName, function, args, startTime, "", "data CRC check failed", err)
		return
	}
	if err := crcMatchesName(readerCopy, "datafile-33-kB"); err != nil {
		logError(testName, function, args, startTime, "", "copy data CRC check failed", err)
		return
	}
	// Close all the get readers before proceeding with CopyObject operations.
	r.Close()
	readerCopy.Close()

	// CopyObject again but with wrong conditions
	src = minio.CopySrcOptions{
		Bucket:               bucketName,
		Object:               objectName,
		MatchUnmodifiedSince: time.Date(2014, time.April, 0, 0, 0, 0, 0, time.UTC),
		NoMatchETag:          objInfo.ETag,
	}

	// Perform the Copy which should fail
	_, err = c.CopyObject(context.Background(), dst, src)
	if err == nil {
		logError(testName, function, args, startTime, "", "CopyObject did not fail for invalid conditions", err)
		return
	}

	src = minio.CopySrcOptions{
		Bucket: bucketName,
		Object: objectName,
	}

	dst = minio.CopyDestOptions{
		Bucket:          bucketName,
		Object:          objectName,
		ReplaceMetadata: true,
		UserMetadata: map[string]string{
			"Copy": "should be same",
		},
	}
	args["dst"] = dst
	args["src"] = src

	_, err = c.CopyObject(context.Background(), dst, src)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObject shouldn't fail", err)
		return
	}

	oi, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}

	stOpts := minio.StatObjectOptions{}
	stOpts.SetMatchETag(oi.ETag)
	objInfo, err = c.StatObject(context.Background(), bucketName, objectName, stOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObject ETag should match and not fail", err)
		return
	}

	if objInfo.Metadata.Get("x-amz-meta-copy") != "should be same" {
		logError(testName, function, args, startTime, "", "CopyObject modified metadata should match", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests SSE-C get object ReaderSeeker interface methods.
func testSSECEncryptedGetObjectReadSeekFunctional() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer func() {
		// Delete all objects and buckets
		if err = cleanupBucket(bucketName, c); err != nil {
			logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
			return
		}
	}()

	// Generate 129MiB of data.
	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{
		ContentType:          "binary/octet-stream",
		ServerSideEncryption: encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+objectName)),
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{
		ServerSideEncryption: encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+objectName)),
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	defer r.Close()

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat object failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(int64(bufSize))+", got "+string(st.Size), err)
		return
	}

	// This following function helps us to compare data from the reader after seek
	// with the data from the original buffer
	cmpData := func(r io.Reader, start, end int) {
		if end-start == 0 {
			return
		}
		buffer := bytes.NewBuffer([]byte{})
		if _, err := io.CopyN(buffer, r, int64(bufSize)); err != nil {
			if err != io.EOF {
				logError(testName, function, args, startTime, "", "CopyN failed", err)
				return
			}
		}
		if !bytes.Equal(buf[start:end], buffer.Bytes()) {
			logError(testName, function, args, startTime, "", "Incorrect read bytes v/s original buffer", err)
			return
		}
	}

	testCases := []struct {
		offset    int64
		whence    int
		pos       int64
		err       error
		shouldCmp bool
		start     int
		end       int
	}{
		// Start from offset 0, fetch data and compare
		{0, 0, 0, nil, true, 0, 0},
		// Start from offset 2048, fetch data and compare
		{2048, 0, 2048, nil, true, 2048, bufSize},
		// Start from offset larger than possible
		{int64(bufSize) + 1024, 0, 0, io.EOF, false, 0, 0},
		// Move to offset 0 without comparing
		{0, 0, 0, nil, false, 0, 0},
		// Move one step forward and compare
		{1, 1, 1, nil, true, 1, bufSize},
		// Move larger than possible
		{int64(bufSize), 1, 0, io.EOF, false, 0, 0},
		// Provide negative offset with CUR_SEEK
		{int64(-1), 1, 0, fmt.Errorf("Negative position not allowed for 1"), false, 0, 0},
		// Test with whence SEEK_END and with positive offset
		{1024, 2, 0, io.EOF, false, 0, 0},
		// Test with whence SEEK_END and with negative offset
		{-1024, 2, int64(bufSize) - 1024, nil, true, bufSize - 1024, bufSize},
		// Test with whence SEEK_END and with large negative offset
		{-int64(bufSize) * 2, 2, 0, fmt.Errorf("Seeking at negative offset not allowed for 2"), false, 0, 0},
		// Test with invalid whence
		{0, 3, 0, fmt.Errorf("Invalid whence 3"), false, 0, 0},
	}

	for i, testCase := range testCases {
		// Perform seek operation
		n, err := r.Seek(testCase.offset, testCase.whence)
		if err != nil && testCase.err == nil {
			// We expected success.
			logError(testName, function, args, startTime, "",
				fmt.Sprintf("Test %d, unexpected err value: expected: %s, found: %s", i+1, testCase.err, err), err)
			return
		}
		if err == nil && testCase.err != nil {
			// We expected failure, but got success.
			logError(testName, function, args, startTime, "",
				fmt.Sprintf("Test %d, unexpected err value: expected: %s, found: %s", i+1, testCase.err, err), err)
			return
		}
		if err != nil && testCase.err != nil {
			if err.Error() != testCase.err.Error() {
				// We expect a specific error
				logError(testName, function, args, startTime, "",
					fmt.Sprintf("Test %d, unexpected err value: expected: %s, found: %s", i+1, testCase.err, err), err)
				return
			}
		}
		// Check the returned seek pos
		if n != testCase.pos {
			logError(testName, function, args, startTime, "",
				fmt.Sprintf("Test %d, number of bytes seeked does not match, expected %d, got %d", i+1, testCase.pos, n), err)
			return
		}
		// Compare only if shouldCmp is activated
		if testCase.shouldCmp {
			cmpData(r, testCase.start, testCase.end)
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Tests SSE-S3 get object ReaderSeeker interface methods.
func testSSES3EncryptedGetObjectReadSeekFunctional() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer func() {
		// Delete all objects and buckets
		if err = cleanupBucket(bucketName, c); err != nil {
			logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
			return
		}
	}()

	// Generate 129MiB of data.
	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{
		ContentType:          "binary/octet-stream",
		ServerSideEncryption: encrypt.NewSSE(),
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	defer r.Close()

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat object failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(int64(bufSize))+", got "+string(st.Size), err)
		return
	}

	// This following function helps us to compare data from the reader after seek
	// with the data from the original buffer
	cmpData := func(r io.Reader, start, end int) {
		if end-start == 0 {
			return
		}
		buffer := bytes.NewBuffer([]byte{})
		if _, err := io.CopyN(buffer, r, int64(bufSize)); err != nil {
			if err != io.EOF {
				logError(testName, function, args, startTime, "", "CopyN failed", err)
				return
			}
		}
		if !bytes.Equal(buf[start:end], buffer.Bytes()) {
			logError(testName, function, args, startTime, "", "Incorrect read bytes v/s original buffer", err)
			return
		}
	}

	testCases := []struct {
		offset    int64
		whence    int
		pos       int64
		err       error
		shouldCmp bool
		start     int
		end       int
	}{
		// Start from offset 0, fetch data and compare
		{0, 0, 0, nil, true, 0, 0},
		// Start from offset 2048, fetch data and compare
		{2048, 0, 2048, nil, true, 2048, bufSize},
		// Start from offset larger than possible
		{int64(bufSize) + 1024, 0, 0, io.EOF, false, 0, 0},
		// Move to offset 0 without comparing
		{0, 0, 0, nil, false, 0, 0},
		// Move one step forward and compare
		{1, 1, 1, nil, true, 1, bufSize},
		// Move larger than possible
		{int64(bufSize), 1, 0, io.EOF, false, 0, 0},
		// Provide negative offset with CUR_SEEK
		{int64(-1), 1, 0, fmt.Errorf("Negative position not allowed for 1"), false, 0, 0},
		// Test with whence SEEK_END and with positive offset
		{1024, 2, 0, io.EOF, false, 0, 0},
		// Test with whence SEEK_END and with negative offset
		{-1024, 2, int64(bufSize) - 1024, nil, true, bufSize - 1024, bufSize},
		// Test with whence SEEK_END and with large negative offset
		{-int64(bufSize) * 2, 2, 0, fmt.Errorf("Seeking at negative offset not allowed for 2"), false, 0, 0},
		// Test with invalid whence
		{0, 3, 0, fmt.Errorf("Invalid whence 3"), false, 0, 0},
	}

	for i, testCase := range testCases {
		// Perform seek operation
		n, err := r.Seek(testCase.offset, testCase.whence)
		if err != nil && testCase.err == nil {
			// We expected success.
			logError(testName, function, args, startTime, "",
				fmt.Sprintf("Test %d, unexpected err value: expected: %s, found: %s", i+1, testCase.err, err), err)
			return
		}
		if err == nil && testCase.err != nil {
			// We expected failure, but got success.
			logError(testName, function, args, startTime, "",
				fmt.Sprintf("Test %d, unexpected err value: expected: %s, found: %s", i+1, testCase.err, err), err)
			return
		}
		if err != nil && testCase.err != nil {
			if err.Error() != testCase.err.Error() {
				// We expect a specific error
				logError(testName, function, args, startTime, "",
					fmt.Sprintf("Test %d, unexpected err value: expected: %s, found: %s", i+1, testCase.err, err), err)
				return
			}
		}
		// Check the returned seek pos
		if n != testCase.pos {
			logError(testName, function, args, startTime, "",
				fmt.Sprintf("Test %d, number of bytes seeked does not match, expected %d, got %d", i+1, testCase.pos, n), err)
			return
		}
		// Compare only if shouldCmp is activated
		if testCase.shouldCmp {
			cmpData(r, testCase.start, testCase.end)
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Tests SSE-C get object ReaderAt interface methods.
func testSSECEncryptedGetObjectReadAtFunctional() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 129MiB of data.
	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{
		ContentType:          "binary/octet-stream",
		ServerSideEncryption: encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+objectName)),
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{
		ServerSideEncryption: encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+objectName)),
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	defer r.Close()

	offset := int64(2048)

	// read directly
	buf1 := make([]byte, 512)
	buf2 := make([]byte, 512)
	buf3 := make([]byte, 512)
	buf4 := make([]byte, 512)

	// Test readAt before stat is called such that objectInfo doesn't change.
	m, err := r.ReadAt(buf1, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf1) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf1))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf1, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes in stat does not match, expected "+string(int64(bufSize))+", got "+string(st.Size), err)
		return
	}

	m, err = r.ReadAt(buf2, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf2) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf2))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf2, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512
	m, err = r.ReadAt(buf3, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf3) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf3))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf3, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512
	m, err = r.ReadAt(buf4, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf4) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf4))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf4, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}

	buf5 := make([]byte, len(buf))
	// Read the whole object.
	m, err = r.ReadAt(buf5, 0)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "ReadAt failed", err)
			return
		}
	}
	if m != len(buf5) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf5))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf, buf5) {
		logError(testName, function, args, startTime, "", "Incorrect data read in GetObject, than what was previously uploaded", err)
		return
	}

	buf6 := make([]byte, len(buf)+1)
	// Read the whole object and beyond.
	_, err = r.ReadAt(buf6, 0)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "ReadAt failed", err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Tests SSE-S3 get object ReaderAt interface methods.
func testSSES3EncryptedGetObjectReadAtFunctional() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 129MiB of data.
	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{
		ContentType:          "binary/octet-stream",
		ServerSideEncryption: encrypt.NewSSE(),
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	defer r.Close()

	offset := int64(2048)

	// read directly
	buf1 := make([]byte, 512)
	buf2 := make([]byte, 512)
	buf3 := make([]byte, 512)
	buf4 := make([]byte, 512)

	// Test readAt before stat is called such that objectInfo doesn't change.
	m, err := r.ReadAt(buf1, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf1) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf1))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf1, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes in stat does not match, expected "+string(int64(bufSize))+", got "+string(st.Size), err)
		return
	}

	m, err = r.ReadAt(buf2, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf2) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf2))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf2, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512
	m, err = r.ReadAt(buf3, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf3) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf3))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf3, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512
	m, err = r.ReadAt(buf4, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf4) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf4))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf4, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}

	buf5 := make([]byte, len(buf))
	// Read the whole object.
	m, err = r.ReadAt(buf5, 0)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "ReadAt failed", err)
			return
		}
	}
	if m != len(buf5) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf5))+", got "+string(m), err)
		return
	}
	if !bytes.Equal(buf, buf5) {
		logError(testName, function, args, startTime, "", "Incorrect data read in GetObject, than what was previously uploaded", err)
		return
	}

	buf6 := make([]byte, len(buf)+1)
	// Read the whole object and beyond.
	_, err = r.ReadAt(buf6, 0)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "ReadAt failed", err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// testSSECEncryptionPutGet tests encryption with customer provided encryption keys
func testSSECEncryptionPutGet() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutEncryptedObject(bucketName, objectName, reader, sse)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"sse":        "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	testCases := []struct {
		buf []byte
	}{
		{buf: bytes.Repeat([]byte("F"), 1)},
		{buf: bytes.Repeat([]byte("F"), 15)},
		{buf: bytes.Repeat([]byte("F"), 16)},
		{buf: bytes.Repeat([]byte("F"), 17)},
		{buf: bytes.Repeat([]byte("F"), 31)},
		{buf: bytes.Repeat([]byte("F"), 32)},
		{buf: bytes.Repeat([]byte("F"), 33)},
		{buf: bytes.Repeat([]byte("F"), 1024)},
		{buf: bytes.Repeat([]byte("F"), 1024*2)},
		{buf: bytes.Repeat([]byte("F"), 1024*1024)},
	}

	const password = "correct horse battery staple" // https://xkcd.com/936/

	for i, testCase := range testCases {
		// Generate a random object name
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName

		// Secured object
		sse := encrypt.DefaultPBKDF([]byte(password), []byte(bucketName+objectName))
		args["sse"] = sse

		// Put encrypted data
		_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(testCase.buf), int64(len(testCase.buf)), minio.PutObjectOptions{ServerSideEncryption: sse})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutEncryptedObject failed", err)
			return
		}

		// Read the data back
		r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{ServerSideEncryption: sse})
		if err != nil {
			logError(testName, function, args, startTime, "", "GetEncryptedObject failed", err)
			return
		}
		defer r.Close()

		// Compare the sent object with the received one
		recvBuffer := bytes.NewBuffer([]byte{})
		if _, err = io.Copy(recvBuffer, r); err != nil {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", error: "+err.Error(), err)
			return
		}
		if recvBuffer.Len() != len(testCase.buf) {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", Number of bytes of received object does not match, expected "+string(len(testCase.buf))+", got "+string(recvBuffer.Len()), err)
			return
		}
		if !bytes.Equal(testCase.buf, recvBuffer.Bytes()) {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", Encrypted sent is not equal to decrypted, expected "+string(testCase.buf)+", got "+string(recvBuffer.Bytes()), err)
			return
		}

		logSuccess(testName, function, args, startTime)

	}

	logSuccess(testName, function, args, startTime)
}

// TestEncryptionFPut tests encryption with customer specified encryption keys
func testSSECEncryptionFPut() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FPutEncryptedObject(bucketName, objectName, filePath, contentType, sse)"
	args := map[string]interface{}{
		"bucketName":  "",
		"objectName":  "",
		"filePath":    "",
		"contentType": "",
		"sse":         "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Object custom metadata
	customContentType := "custom/contenttype"
	args["metadata"] = customContentType

	testCases := []struct {
		buf []byte
	}{
		{buf: bytes.Repeat([]byte("F"), 0)},
		{buf: bytes.Repeat([]byte("F"), 1)},
		{buf: bytes.Repeat([]byte("F"), 15)},
		{buf: bytes.Repeat([]byte("F"), 16)},
		{buf: bytes.Repeat([]byte("F"), 17)},
		{buf: bytes.Repeat([]byte("F"), 31)},
		{buf: bytes.Repeat([]byte("F"), 32)},
		{buf: bytes.Repeat([]byte("F"), 33)},
		{buf: bytes.Repeat([]byte("F"), 1024)},
		{buf: bytes.Repeat([]byte("F"), 1024*2)},
		{buf: bytes.Repeat([]byte("F"), 1024*1024)},
	}

	const password = "correct horse battery staple" // https://xkcd.com/936/
	for i, testCase := range testCases {
		// Generate a random object name
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName

		// Secured object
		sse := encrypt.DefaultPBKDF([]byte(password), []byte(bucketName+objectName))
		args["sse"] = sse

		// Generate a random file name.
		fileName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		file, err := os.Create(fileName)
		if err != nil {
			logError(testName, function, args, startTime, "", "file create failed", err)
			return
		}
		_, err = file.Write(testCase.buf)
		if err != nil {
			logError(testName, function, args, startTime, "", "file write failed", err)
			return
		}
		file.Close()
		// Put encrypted data
		if _, err = c.FPutObject(context.Background(), bucketName, objectName, fileName, minio.PutObjectOptions{ServerSideEncryption: sse}); err != nil {
			logError(testName, function, args, startTime, "", "FPutEncryptedObject failed", err)
			return
		}

		// Read the data back
		r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{ServerSideEncryption: sse})
		if err != nil {
			logError(testName, function, args, startTime, "", "GetEncryptedObject failed", err)
			return
		}
		defer r.Close()

		// Compare the sent object with the received one
		recvBuffer := bytes.NewBuffer([]byte{})
		if _, err = io.Copy(recvBuffer, r); err != nil {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", error: "+err.Error(), err)
			return
		}
		if recvBuffer.Len() != len(testCase.buf) {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", Number of bytes of received object does not match, expected "+string(len(testCase.buf))+", got "+string(recvBuffer.Len()), err)
			return
		}
		if !bytes.Equal(testCase.buf, recvBuffer.Bytes()) {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", Encrypted sent is not equal to decrypted, expected "+string(testCase.buf)+", got "+string(recvBuffer.Bytes()), err)
			return
		}

		os.Remove(fileName)
	}

	logSuccess(testName, function, args, startTime)
}

// testSSES3EncryptionPutGet tests SSE-S3 encryption
func testSSES3EncryptionPutGet() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutEncryptedObject(bucketName, objectName, reader, sse)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"sse":        "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	testCases := []struct {
		buf []byte
	}{
		{buf: bytes.Repeat([]byte("F"), 1)},
		{buf: bytes.Repeat([]byte("F"), 15)},
		{buf: bytes.Repeat([]byte("F"), 16)},
		{buf: bytes.Repeat([]byte("F"), 17)},
		{buf: bytes.Repeat([]byte("F"), 31)},
		{buf: bytes.Repeat([]byte("F"), 32)},
		{buf: bytes.Repeat([]byte("F"), 33)},
		{buf: bytes.Repeat([]byte("F"), 1024)},
		{buf: bytes.Repeat([]byte("F"), 1024*2)},
		{buf: bytes.Repeat([]byte("F"), 1024*1024)},
	}

	for i, testCase := range testCases {
		// Generate a random object name
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName

		// Secured object
		sse := encrypt.NewSSE()
		args["sse"] = sse

		// Put encrypted data
		_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(testCase.buf), int64(len(testCase.buf)), minio.PutObjectOptions{ServerSideEncryption: sse})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutEncryptedObject failed", err)
			return
		}

		// Read the data back without any encryption headers
		r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "GetEncryptedObject failed", err)
			return
		}
		defer r.Close()

		// Compare the sent object with the received one
		recvBuffer := bytes.NewBuffer([]byte{})
		if _, err = io.Copy(recvBuffer, r); err != nil {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", error: "+err.Error(), err)
			return
		}
		if recvBuffer.Len() != len(testCase.buf) {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", Number of bytes of received object does not match, expected "+string(len(testCase.buf))+", got "+string(recvBuffer.Len()), err)
			return
		}
		if !bytes.Equal(testCase.buf, recvBuffer.Bytes()) {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", Encrypted sent is not equal to decrypted, expected "+string(testCase.buf)+", got "+string(recvBuffer.Bytes()), err)
			return
		}

		logSuccess(testName, function, args, startTime)

	}

	logSuccess(testName, function, args, startTime)
}

// TestSSES3EncryptionFPut tests server side encryption
func testSSES3EncryptionFPut() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FPutEncryptedObject(bucketName, objectName, filePath, contentType, sse)"
	args := map[string]interface{}{
		"bucketName":  "",
		"objectName":  "",
		"filePath":    "",
		"contentType": "",
		"sse":         "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Object custom metadata
	customContentType := "custom/contenttype"
	args["metadata"] = customContentType

	testCases := []struct {
		buf []byte
	}{
		{buf: bytes.Repeat([]byte("F"), 0)},
		{buf: bytes.Repeat([]byte("F"), 1)},
		{buf: bytes.Repeat([]byte("F"), 15)},
		{buf: bytes.Repeat([]byte("F"), 16)},
		{buf: bytes.Repeat([]byte("F"), 17)},
		{buf: bytes.Repeat([]byte("F"), 31)},
		{buf: bytes.Repeat([]byte("F"), 32)},
		{buf: bytes.Repeat([]byte("F"), 33)},
		{buf: bytes.Repeat([]byte("F"), 1024)},
		{buf: bytes.Repeat([]byte("F"), 1024*2)},
		{buf: bytes.Repeat([]byte("F"), 1024*1024)},
	}

	for i, testCase := range testCases {
		// Generate a random object name
		objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		args["objectName"] = objectName

		// Secured object
		sse := encrypt.NewSSE()
		args["sse"] = sse

		// Generate a random file name.
		fileName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
		file, err := os.Create(fileName)
		if err != nil {
			logError(testName, function, args, startTime, "", "file create failed", err)
			return
		}
		_, err = file.Write(testCase.buf)
		if err != nil {
			logError(testName, function, args, startTime, "", "file write failed", err)
			return
		}
		file.Close()
		// Put encrypted data
		if _, err = c.FPutObject(context.Background(), bucketName, objectName, fileName, minio.PutObjectOptions{ServerSideEncryption: sse}); err != nil {
			logError(testName, function, args, startTime, "", "FPutEncryptedObject failed", err)
			return
		}

		// Read the data back
		r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "GetEncryptedObject failed", err)
			return
		}
		defer r.Close()

		// Compare the sent object with the received one
		recvBuffer := bytes.NewBuffer([]byte{})
		if _, err = io.Copy(recvBuffer, r); err != nil {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", error: "+err.Error(), err)
			return
		}
		if recvBuffer.Len() != len(testCase.buf) {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", Number of bytes of received object does not match, expected "+string(len(testCase.buf))+", got "+string(recvBuffer.Len()), err)
			return
		}
		if !bytes.Equal(testCase.buf, recvBuffer.Bytes()) {
			logError(testName, function, args, startTime, "", "Test "+string(i+1)+", Encrypted sent is not equal to decrypted, expected "+string(testCase.buf)+", got "+string(recvBuffer.Bytes()), err)
			return
		}

		os.Remove(fileName)
	}

	logSuccess(testName, function, args, startTime)
}

func testBucketNotification() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "SetBucketNotification(bucketName)"
	args := map[string]interface{}{
		"bucketName": "",
	}

	if os.Getenv("NOTIFY_BUCKET") == "" ||
		os.Getenv("NOTIFY_SERVICE") == "" ||
		os.Getenv("NOTIFY_REGION") == "" ||
		os.Getenv("NOTIFY_ACCOUNTID") == "" ||
		os.Getenv("NOTIFY_RESOURCE") == "" {
		logIgnored(testName, function, args, startTime, "Skipped notification test as it is not configured")
		return
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	bucketName := os.Getenv("NOTIFY_BUCKET")
	args["bucketName"] = bucketName

	topicArn := notification.NewArn("aws", os.Getenv("NOTIFY_SERVICE"), os.Getenv("NOTIFY_REGION"), os.Getenv("NOTIFY_ACCOUNTID"), os.Getenv("NOTIFY_RESOURCE"))
	queueArn := notification.NewArn("aws", "dummy-service", "dummy-region", "dummy-accountid", "dummy-resource")

	topicConfig := notification.NewConfig(topicArn)
	topicConfig.AddEvents(notification.ObjectCreatedAll, notification.ObjectRemovedAll)
	topicConfig.AddFilterSuffix("jpg")

	queueConfig := notification.NewConfig(queueArn)
	queueConfig.AddEvents(notification.ObjectCreatedAll)
	queueConfig.AddFilterPrefix("photos/")

	config := notification.Configuration{}
	config.AddTopic(topicConfig)

	// Add the same topicConfig again, should have no effect
	// because it is duplicated
	config.AddTopic(topicConfig)
	if len(config.TopicConfigs) != 1 {
		logError(testName, function, args, startTime, "", "Duplicate entry added", err)
		return
	}

	// Add and remove a queue config
	config.AddQueue(queueConfig)
	config.RemoveQueueByArn(queueArn)

	err = c.SetBucketNotification(context.Background(), bucketName, config)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketNotification failed", err)
		return
	}

	config, err = c.GetBucketNotification(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketNotification failed", err)
		return
	}

	if len(config.TopicConfigs) != 1 {
		logError(testName, function, args, startTime, "", "Topic config is empty", err)
		return
	}

	if config.TopicConfigs[0].Filter.S3Key.FilterRules[0].Value != "jpg" {
		logError(testName, function, args, startTime, "", "Couldn't get the suffix", err)
		return
	}

	err = c.RemoveAllBucketNotification(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "RemoveAllBucketNotification failed", err)
		return
	}

	// Delete all objects and buckets
	if err = cleanupBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests comprehensive list of all methods.
func testFunctional() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "testFunctional()"
	functionAll := ""
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, nil, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	// Make a new bucket.
	function = "MakeBucket(bucketName, region)"
	functionAll = "MakeBucket(bucketName, region)"
	args["bucketName"] = bucketName
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})

	defer cleanupBucket(bucketName, c)
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	// Generate a random file name.
	fileName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	file, err := os.Create(fileName)
	if err != nil {
		logError(testName, function, args, startTime, "", "File creation failed", err)
		return
	}
	for i := 0; i < 3; i++ {
		buf := make([]byte, rand.Intn(1<<19))
		_, err = file.Write(buf)
		if err != nil {
			logError(testName, function, args, startTime, "", "File write failed", err)
			return
		}
	}
	file.Close()

	// Verify if bucket exits and you have access.
	var exists bool
	function = "BucketExists(bucketName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
	}
	exists, err = c.BucketExists(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "BucketExists failed", err)
		return
	}
	if !exists {
		logError(testName, function, args, startTime, "", "Could not find the bucket", err)
		return
	}

	// Asserting the default bucket policy.
	function = "GetBucketPolicy(ctx, bucketName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
	}
	nilPolicy, err := c.GetBucketPolicy(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketPolicy failed", err)
		return
	}
	if nilPolicy != "" {
		logError(testName, function, args, startTime, "", "policy should be set to nil", err)
		return
	}

	// Set the bucket policy to 'public readonly'.
	function = "SetBucketPolicy(bucketName, readOnlyPolicy)"
	functionAll += ", " + function

	readOnlyPolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::` + bucketName + `"]}]}`
	args = map[string]interface{}{
		"bucketName":   bucketName,
		"bucketPolicy": readOnlyPolicy,
	}

	err = c.SetBucketPolicy(context.Background(), bucketName, readOnlyPolicy)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketPolicy failed", err)
		return
	}
	// should return policy `readonly`.
	function = "GetBucketPolicy(ctx, bucketName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
	}
	_, err = c.GetBucketPolicy(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketPolicy failed", err)
		return
	}

	// Make the bucket 'public writeonly'.
	function = "SetBucketPolicy(bucketName, writeOnlyPolicy)"
	functionAll += ", " + function

	writeOnlyPolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::` + bucketName + `"]}]}`
	args = map[string]interface{}{
		"bucketName":   bucketName,
		"bucketPolicy": writeOnlyPolicy,
	}
	err = c.SetBucketPolicy(context.Background(), bucketName, writeOnlyPolicy)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketPolicy failed", err)
		return
	}
	// should return policy `writeonly`.
	function = "GetBucketPolicy(ctx, bucketName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
	}

	_, err = c.GetBucketPolicy(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketPolicy failed", err)
		return
	}

	// Make the bucket 'public read/write'.
	function = "SetBucketPolicy(bucketName, readWritePolicy)"
	functionAll += ", " + function

	readWritePolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:ListBucket","s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::` + bucketName + `"]}]}`

	args = map[string]interface{}{
		"bucketName":   bucketName,
		"bucketPolicy": readWritePolicy,
	}
	err = c.SetBucketPolicy(context.Background(), bucketName, readWritePolicy)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketPolicy failed", err)
		return
	}
	// should return policy `readwrite`.
	function = "GetBucketPolicy(bucketName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
	}
	_, err = c.GetBucketPolicy(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketPolicy failed", err)
		return
	}

	// List all buckets.
	function = "ListBuckets()"
	functionAll += ", " + function
	args = nil
	buckets, err := c.ListBuckets(context.Background())

	if len(buckets) == 0 {
		logError(testName, function, args, startTime, "", "Found bucket list to be empty", err)
		return
	}
	if err != nil {
		logError(testName, function, args, startTime, "", "ListBuckets failed", err)
		return
	}

	// Verify if previously created bucket is listed in list buckets.
	bucketFound := false
	for _, bucket := range buckets {
		if bucket.Name == bucketName {
			bucketFound = true
		}
	}

	// If bucket not found error out.
	if !bucketFound {
		logError(testName, function, args, startTime, "", "Bucket: "+bucketName+" not found", err)
		return
	}

	objectName := bucketName + "unique"

	// Generate data
	buf := bytes.Repeat([]byte("f"), 1<<19)

	function = "PutObject(bucketName, objectName, reader, contentType)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName":  bucketName,
		"objectName":  objectName,
		"contentType": "",
	}

	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	args = map[string]interface{}{
		"bucketName":  bucketName,
		"objectName":  objectName + "-nolength",
		"contentType": "binary/octet-stream",
	}

	_, err = c.PutObject(context.Background(), bucketName, objectName+"-nolength", bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Instantiate a done channel to close all listing.
	doneCh := make(chan struct{})
	defer close(doneCh)

	objFound := false
	isRecursive := true // Recursive is true.

	function = "ListObjects(bucketName, objectName, isRecursive, doneCh)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName":  bucketName,
		"objectName":  objectName,
		"isRecursive": isRecursive,
	}

	for obj := range c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{UseV1: true, Prefix: objectName, Recursive: true}) {
		if obj.Key == objectName {
			objFound = true
			break
		}
	}
	if !objFound {
		logError(testName, function, args, startTime, "", "Object "+objectName+" not found", err)
		return
	}

	objFound = false
	isRecursive = true // Recursive is true.
	function = "ListObjects()"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName":  bucketName,
		"objectName":  objectName,
		"isRecursive": isRecursive,
	}

	for obj := range c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{Prefix: objectName, Recursive: isRecursive}) {
		if obj.Key == objectName {
			objFound = true
			break
		}
	}
	if !objFound {
		logError(testName, function, args, startTime, "", "Object "+objectName+" not found", err)
		return
	}

	incompObjNotFound := true

	function = "ListIncompleteUploads(bucketName, objectName, isRecursive, doneCh)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName":  bucketName,
		"objectName":  objectName,
		"isRecursive": isRecursive,
	}

	for objIncompl := range c.ListIncompleteUploads(context.Background(), bucketName, objectName, isRecursive) {
		if objIncompl.Key != "" {
			incompObjNotFound = false
			break
		}
	}
	if !incompObjNotFound {
		logError(testName, function, args, startTime, "", "Unexpected dangling incomplete upload found", err)
		return
	}

	function = "GetObject(bucketName, objectName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
	}
	newReader, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	newReadBytes, err := io.ReadAll(newReader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	if !bytes.Equal(newReadBytes, buf) {
		logError(testName, function, args, startTime, "", "GetObject bytes mismatch", err)
		return
	}
	newReader.Close()

	function = "FGetObject(bucketName, objectName, fileName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
		"fileName":   fileName + "-f",
	}
	err = c.FGetObject(context.Background(), bucketName, objectName, fileName+"-f", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FGetObject failed", err)
		return
	}

	function = "PresignedHeadObject(bucketName, objectName, expires, reqParams)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": "",
		"expires":    3600 * time.Second,
	}
	if _, err = c.PresignedHeadObject(context.Background(), bucketName, "", 3600*time.Second, nil); err == nil {
		logError(testName, function, args, startTime, "", "PresignedHeadObject success", err)
		return
	}

	// Generate presigned HEAD object url.
	function = "PresignedHeadObject(bucketName, objectName, expires, reqParams)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
		"expires":    3600 * time.Second,
	}
	presignedHeadURL, err := c.PresignedHeadObject(context.Background(), bucketName, objectName, 3600*time.Second, nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedHeadObject failed", err)
		return
	}

	transport := createHTTPTransport()
	if err != nil {
		logError(testName, function, args, startTime, "", "DefaultTransport failed", err)
		return
	}

	httpClient := &http.Client{
		// Setting a sensible time out of 30secs to wait for response
		// headers. Request is pro-actively canceled after 30secs
		// with no response.
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	req, err := http.NewRequest(http.MethodHead, presignedHeadURL.String(), nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedHeadObject request was incorrect", err)
		return
	}

	// Verify if presigned url works.
	resp, err := httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedHeadObject response incorrect", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		logError(testName, function, args, startTime, "", "PresignedHeadObject response incorrect, status "+string(resp.StatusCode), err)
		return
	}
	if resp.Header.Get("ETag") == "" {
		logError(testName, function, args, startTime, "", "PresignedHeadObject response incorrect", err)
		return
	}
	resp.Body.Close()

	function = "PresignedGetObject(bucketName, objectName, expires, reqParams)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": "",
		"expires":    3600 * time.Second,
	}
	_, err = c.PresignedGetObject(context.Background(), bucketName, "", 3600*time.Second, nil)
	if err == nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject success", err)
		return
	}

	// Generate presigned GET object url.
	function = "PresignedGetObject(bucketName, objectName, expires, reqParams)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
		"expires":    3600 * time.Second,
	}
	presignedGetURL, err := c.PresignedGetObject(context.Background(), bucketName, objectName, 3600*time.Second, nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject failed", err)
		return
	}

	// Verify if presigned url works.
	req, err = http.NewRequest(http.MethodGet, presignedGetURL.String(), nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject request incorrect", err)
		return
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect, status "+string(resp.StatusCode), err)
		return
	}
	newPresignedBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect", err)
		return
	}
	resp.Body.Close()
	if !bytes.Equal(newPresignedBytes, buf) {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect", err)
		return
	}

	// Set request parameters.
	reqParams := make(url.Values)
	reqParams.Set("response-content-disposition", "attachment; filename=\"test.txt\"")
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
		"expires":    3600 * time.Second,
		"reqParams":  reqParams,
	}
	presignedGetURL, err = c.PresignedGetObject(context.Background(), bucketName, objectName, 3600*time.Second, reqParams)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject failed", err)
		return
	}

	// Verify if presigned url works.
	req, err = http.NewRequest(http.MethodGet, presignedGetURL.String(), nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject request incorrect", err)
		return
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect, status "+string(resp.StatusCode), err)
		return
	}
	newPresignedBytes, err = io.ReadAll(resp.Body)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect", err)
		return
	}
	if !bytes.Equal(newPresignedBytes, buf) {
		logError(testName, function, args, startTime, "", "Bytes mismatch for presigned GET URL", err)
		return
	}
	if resp.Header.Get("Content-Disposition") != "attachment; filename=\"test.txt\"" {
		logError(testName, function, args, startTime, "", "wrong Content-Disposition received "+string(resp.Header.Get("Content-Disposition")), err)
		return
	}

	function = "PresignedPutObject(bucketName, objectName, expires)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": "",
		"expires":    3600 * time.Second,
	}
	_, err = c.PresignedPutObject(context.Background(), bucketName, "", 3600*time.Second)
	if err == nil {
		logError(testName, function, args, startTime, "", "PresignedPutObject success", err)
		return
	}

	function = "PresignedPutObject(bucketName, objectName, expires)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName + "-presigned",
		"expires":    3600 * time.Second,
	}
	presignedPutURL, err := c.PresignedPutObject(context.Background(), bucketName, objectName+"-presigned", 3600*time.Second)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedPutObject failed", err)
		return
	}

	buf = bytes.Repeat([]byte("g"), 1<<19)

	req, err = http.NewRequest(http.MethodPut, presignedPutURL.String(), bytes.NewReader(buf))
	if err != nil {
		logError(testName, function, args, startTime, "", "Couldn't make HTTP request with PresignedPutObject URL", err)
		return
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedPutObject failed", err)
		return
	}

	newReader, err = c.GetObject(context.Background(), bucketName, objectName+"-presigned", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject after PresignedPutObject failed", err)
		return
	}

	newReadBytes, err = io.ReadAll(newReader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll after GetObject failed", err)
		return
	}

	if !bytes.Equal(newReadBytes, buf) {
		logError(testName, function, args, startTime, "", "Bytes mismatch", err)
		return
	}

	function = "PresignHeader(method, bucketName, objectName, expires, reqParams, extraHeaders)"
	functionAll += ", " + function
	presignExtraHeaders := map[string][]string{
		"mysecret": {"abcxxx"},
	}
	args = map[string]interface{}{
		"method":       "PUT",
		"bucketName":   bucketName,
		"objectName":   objectName + "-presign-custom",
		"expires":      3600 * time.Second,
		"extraHeaders": presignExtraHeaders,
	}
	presignedURL, err := c.PresignHeader(context.Background(), "PUT", bucketName, objectName+"-presign-custom", 3600*time.Second, nil, presignExtraHeaders)
	if err != nil {
		logError(testName, function, args, startTime, "", "Presigned failed", err)
		return
	}

	// Generate data more than 32K
	buf = bytes.Repeat([]byte("1"), rand.Intn(1<<10)+32*1024)

	req, err = http.NewRequest(http.MethodPut, presignedURL.String(), bytes.NewReader(buf))
	if err != nil {
		logError(testName, function, args, startTime, "", "HTTP request to Presigned URL failed", err)
		return
	}

	req.Header.Add("mysecret", "abcxxx")
	resp, err = httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "HTTP request to Presigned URL failed", err)
		return
	}

	// Download the uploaded object to verify
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName + "-presign-custom",
	}
	newReader, err = c.GetObject(context.Background(), bucketName, objectName+"-presign-custom", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject of uploaded custom-presigned object failed", err)
		return
	}

	newReadBytes, err = io.ReadAll(newReader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed during get on custom-presigned put object", err)
		return
	}
	newReader.Close()

	if !bytes.Equal(newReadBytes, buf) {
		logError(testName, function, args, startTime, "", "Bytes mismatch on custom-presigned object upload verification", err)
		return
	}

	function = "RemoveObject(bucketName, objectName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
	}
	err = c.RemoveObject(context.Background(), bucketName, objectName, minio.RemoveObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "RemoveObject failed", err)
		return
	}
	args["objectName"] = objectName + "-f"
	err = c.RemoveObject(context.Background(), bucketName, objectName+"-f", minio.RemoveObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "RemoveObject failed", err)
		return
	}

	args["objectName"] = objectName + "-nolength"
	err = c.RemoveObject(context.Background(), bucketName, objectName+"-nolength", minio.RemoveObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "RemoveObject failed", err)
		return
	}

	args["objectName"] = objectName + "-presigned"
	err = c.RemoveObject(context.Background(), bucketName, objectName+"-presigned", minio.RemoveObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "RemoveObject failed", err)
		return
	}

	args["objectName"] = objectName + "-presign-custom"
	err = c.RemoveObject(context.Background(), bucketName, objectName+"-presign-custom", minio.RemoveObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "RemoveObject failed", err)
		return
	}

	function = "RemoveBucket(bucketName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
	}
	err = c.RemoveBucket(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "RemoveBucket failed", err)
		return
	}
	err = c.RemoveBucket(context.Background(), bucketName)
	if err == nil {
		logError(testName, function, args, startTime, "", "RemoveBucket did not fail for invalid bucket name", err)
		return
	}
	if err.Error() != "The specified bucket does not exist" {
		logError(testName, function, args, startTime, "", "RemoveBucket failed", err)
		return
	}

	os.Remove(fileName)
	os.Remove(fileName + "-f")
	logSuccess(testName, functionAll, args, startTime)
}

// Test for validating GetObject Reader* methods functioning when the
// object is modified in the object store.
func testGetObjectModified() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Make a new bucket.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Upload an object.
	objectName := "myobject"
	args["objectName"] = objectName
	content := "helloworld"
	_, err = c.PutObject(context.Background(), bucketName, objectName, strings.NewReader(content), int64(len(content)), minio.PutObjectOptions{ContentType: "application/text"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Failed to upload "+objectName+", to bucket "+bucketName, err)
		return
	}

	defer c.RemoveObject(context.Background(), bucketName, objectName, minio.RemoveObjectOptions{})

	reader, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Failed to GetObject "+objectName+", from bucket "+bucketName, err)
		return
	}
	defer reader.Close()

	// Read a few bytes of the object.
	b := make([]byte, 5)
	n, err := reader.ReadAt(b, 0)
	if err != nil {
		logError(testName, function, args, startTime, "", "Failed to read object "+objectName+", from bucket "+bucketName+" at an offset", err)
		return
	}

	// Upload different contents to the same object while object is being read.
	newContent := "goodbyeworld"
	_, err = c.PutObject(context.Background(), bucketName, objectName, strings.NewReader(newContent), int64(len(newContent)), minio.PutObjectOptions{ContentType: "application/text"})
	if err != nil {
		logError(testName, function, args, startTime, "", "Failed to upload "+objectName+", to bucket "+bucketName, err)
		return
	}

	// Confirm that a Stat() call in between doesn't change the Object's cached etag.
	_, err = reader.Stat()
	expectedError := "At least one of the pre-conditions you specified did not hold."
	if err.Error() != expectedError {
		logError(testName, function, args, startTime, "", "Expected Stat to fail with error "+expectedError+", but received "+err.Error(), err)
		return
	}

	// Read again only to find object contents have been modified since last read.
	_, err = reader.ReadAt(b, int64(n))
	if err.Error() != expectedError {
		logError(testName, function, args, startTime, "", "Expected ReadAt to fail with error "+expectedError+", but received "+err.Error(), err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test validates putObject to upload a file seeked at a given offset.
func testPutObjectUploadSeekedObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, fileToUpload, contentType)"
	args := map[string]interface{}{
		"bucketName":   "",
		"objectName":   "",
		"fileToUpload": "",
		"contentType":  "binary/octet-stream",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Make a new bucket.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, c)

	var tempfile *os.File

	if fileName := getMintDataDirFilePath("datafile-100-kB"); fileName != "" {
		tempfile, err = os.Open(fileName)
		if err != nil {
			logError(testName, function, args, startTime, "", "File open failed", err)
			return
		}
		args["fileToUpload"] = fileName
	} else {
		tempfile, err = os.CreateTemp("", "minio-go-upload-test-")
		if err != nil {
			logError(testName, function, args, startTime, "", "TempFile create failed", err)
			return
		}
		args["fileToUpload"] = tempfile.Name()

		// Generate 100kB data
		if _, err = io.Copy(tempfile, getDataReader("datafile-100-kB")); err != nil {
			logError(testName, function, args, startTime, "", "File copy failed", err)
			return
		}

		defer os.Remove(tempfile.Name())

		// Seek back to the beginning of the file.
		tempfile.Seek(0, 0)
	}
	length := 100 * humanize.KiByte
	objectName := fmt.Sprintf("test-file-%v", rand.Uint32())
	args["objectName"] = objectName

	offset := length / 2
	if _, err = tempfile.Seek(int64(offset), 0); err != nil {
		logError(testName, function, args, startTime, "", "TempFile seek failed", err)
		return
	}

	_, err = c.PutObject(context.Background(), bucketName, objectName, tempfile, int64(length-offset), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	tempfile.Close()

	obj, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	defer obj.Close()

	n, err := obj.Seek(int64(offset), 0)
	if err != nil {
		logError(testName, function, args, startTime, "", "Seek failed", err)
		return
	}
	if n != int64(offset) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Invalid offset returned, expected %d got %d", int64(offset), n), err)
		return
	}

	_, err = c.PutObject(context.Background(), bucketName, objectName+"getobject", obj, int64(length-offset), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	st, err := c.StatObject(context.Background(), bucketName, objectName+"getobject", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}
	if st.Size != int64(length-offset) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Invalid offset returned, expected %d got %d", int64(length-offset), n), err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests bucket re-create errors.
func testMakeBucketErrorV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "MakeBucket(bucketName, region)"
	args := map[string]interface{}{
		"bucketName": "",
		"region":     "eu-west-1",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	region := "eu-west-1"
	args["bucketName"] = bucketName
	args["region"] = region

	// Make a new bucket in 'eu-west-1'.
	if err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: region}); err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	if err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: region}); err == nil {
		logError(testName, function, args, startTime, "", "MakeBucket did not fail for existing bucket name", err)
		return
	}
	// Verify valid error response from server.
	if minio.ToErrorResponse(err).Code != minio.BucketAlreadyExists &&
		minio.ToErrorResponse(err).Code != minio.BucketAlreadyOwnedByYou {
		logError(testName, function, args, startTime, "", "Invalid error returned by server", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test get object reader to not throw error on being closed twice.
func testGetObjectClosedTwiceV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "MakeBucket(bucketName, region)"
	args := map[string]interface{}{
		"bucketName": "",
		"region":     "eu-west-1",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(bufSize)+" got "+string(st.Size), err)
		return
	}
	if err := r.Close(); err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	if err := r.Close(); err == nil {
		logError(testName, function, args, startTime, "", "Object is already closed, should return error", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests FPutObject hidden contentType setting
func testFPutObjectV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FPutObject(bucketName, objectName, fileName, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"fileName":   "",
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Make a temp file with 11*1024*1024 bytes of data.
	file, err := os.CreateTemp(os.TempDir(), "FPutObjectTest")
	if err != nil {
		logError(testName, function, args, startTime, "", "TempFile creation failed", err)
		return
	}

	r := bytes.NewReader(bytes.Repeat([]byte("b"), 11*1024*1024))
	n, err := io.CopyN(file, r, 11*1024*1024)
	if err != nil {
		logError(testName, function, args, startTime, "", "Copy failed", err)
		return
	}
	if n != int64(11*1024*1024) {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(int64(11*1024*1024))+" got "+string(n), err)
		return
	}

	// Close the file pro-actively for windows.
	err = file.Close()
	if err != nil {
		logError(testName, function, args, startTime, "", "File close failed", err)
		return
	}

	// Set base object name
	objectName := bucketName + "FPutObject"
	args["objectName"] = objectName
	args["fileName"] = file.Name()

	// Perform standard FPutObject with contentType provided (Expecting application/octet-stream)
	_, err = c.FPutObject(context.Background(), bucketName, objectName+"-standard", file.Name(), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "FPutObject failed", err)
		return
	}

	// Perform FPutObject with no contentType provided (Expecting application/octet-stream)
	args["objectName"] = objectName + "-Octet"
	args["contentType"] = ""

	_, err = c.FPutObject(context.Background(), bucketName, objectName+"-Octet", file.Name(), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FPutObject failed", err)
		return
	}

	// Add extension to temp file name
	fileName := file.Name()
	err = os.Rename(fileName, fileName+".gtar")
	if err != nil {
		logError(testName, function, args, startTime, "", "Rename failed", err)
		return
	}

	// Perform FPutObject with no contentType provided (Expecting application/x-gtar)
	args["objectName"] = objectName + "-Octet"
	args["contentType"] = ""
	args["fileName"] = fileName + ".gtar"

	_, err = c.FPutObject(context.Background(), bucketName, objectName+"-GTar", fileName+".gtar", minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FPutObject failed", err)
		return
	}

	// Check headers and sizes
	rStandard, err := c.StatObject(context.Background(), bucketName, objectName+"-standard", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}

	if rStandard.Size != 11*1024*1024 {
		logError(testName, function, args, startTime, "", "Unexpected size", nil)
		return
	}

	if rStandard.ContentType != "application/octet-stream" {
		logError(testName, function, args, startTime, "", "Content-Type headers mismatched, expected: application/octet-stream , got "+rStandard.ContentType, err)
		return
	}

	rOctet, err := c.StatObject(context.Background(), bucketName, objectName+"-Octet", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}
	if rOctet.ContentType != "application/octet-stream" {
		logError(testName, function, args, startTime, "", "Content-Type headers mismatched, expected: application/octet-stream , got "+rOctet.ContentType, err)
		return
	}

	if rOctet.Size != 11*1024*1024 {
		logError(testName, function, args, startTime, "", "Unexpected size", nil)
		return
	}

	rGTar, err := c.StatObject(context.Background(), bucketName, objectName+"-GTar", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}
	if rGTar.Size != 11*1024*1024 {
		logError(testName, function, args, startTime, "", "Unexpected size", nil)
		return
	}
	if rGTar.ContentType != "application/x-gtar" && rGTar.ContentType != "application/octet-stream" && rGTar.ContentType != "application/x-tar" {
		logError(testName, function, args, startTime, "", "Content-Type headers mismatched, expected: application/x-tar , got "+rGTar.ContentType, err)
		return
	}

	os.Remove(fileName + ".gtar")
	logSuccess(testName, function, args, startTime)
}

// Tests various bucket supported formats.
func testMakeBucketRegionsV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "MakeBucket(bucketName, region)"
	args := map[string]interface{}{
		"bucketName": "",
		"region":     "eu-west-1",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket in 'eu-central-1'.
	if err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "eu-west-1"}); err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	if err = cleanupBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed while removing bucket recursively", err)
		return
	}

	// Make a new bucket with '.' in its name, in 'us-west-2'. This
	// request is internally staged into a path style instead of
	// virtual host style.
	if err = c.MakeBucket(context.Background(), bucketName+".withperiod", minio.MakeBucketOptions{Region: "us-west-2"}); err != nil {
		args["bucketName"] = bucketName + ".withperiod"
		args["region"] = "us-west-2"
		logError(testName, function, args, startTime, "", "MakeBucket test with a bucket name with period, '.', failed", err)
		return
	}

	// Delete all objects and buckets
	if err = cleanupBucket(bucketName+".withperiod", c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed while removing bucket recursively", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests get object ReaderSeeker interface methods.
func testGetObjectReadSeekFunctionalV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data.
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	defer r.Close()

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes in stat does not match, expected "+string(int64(bufSize))+" got "+string(st.Size), err)
		return
	}

	offset := int64(2048)
	n, err := r.Seek(offset, 0)
	if err != nil {
		logError(testName, function, args, startTime, "", "Seek failed", err)
		return
	}
	if n != offset {
		logError(testName, function, args, startTime, "", "Number of seeked bytes does not match, expected "+string(offset)+" got "+string(n), err)
		return
	}
	n, err = r.Seek(0, 1)
	if err != nil {
		logError(testName, function, args, startTime, "", "Seek failed", err)
		return
	}
	if n != offset {
		logError(testName, function, args, startTime, "", "Number of seeked bytes does not match, expected "+string(offset)+" got "+string(n), err)
		return
	}
	_, err = r.Seek(offset, 2)
	if err == nil {
		logError(testName, function, args, startTime, "", "Seek on positive offset for whence '2' should error out", err)
		return
	}
	n, err = r.Seek(-offset, 2)
	if err != nil {
		logError(testName, function, args, startTime, "", "Seek failed", err)
		return
	}
	if n != st.Size-offset {
		logError(testName, function, args, startTime, "", "Number of seeked bytes does not match, expected "+string(st.Size-offset)+" got "+string(n), err)
		return
	}

	var buffer1 bytes.Buffer
	if _, err = io.CopyN(&buffer1, r, st.Size); err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "Copy failed", err)
			return
		}
	}
	if !bytes.Equal(buf[len(buf)-int(offset):], buffer1.Bytes()) {
		logError(testName, function, args, startTime, "", "Incorrect read bytes v/s original buffer", err)
		return
	}

	// Seek again and read again.
	n, err = r.Seek(offset-1, 0)
	if err != nil {
		logError(testName, function, args, startTime, "", "Seek failed", err)
		return
	}
	if n != (offset - 1) {
		logError(testName, function, args, startTime, "", "Number of seeked bytes does not match, expected "+string(offset-1)+" got "+string(n), err)
		return
	}

	var buffer2 bytes.Buffer
	if _, err = io.CopyN(&buffer2, r, st.Size); err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "Copy failed", err)
			return
		}
	}
	// Verify now lesser bytes.
	if !bytes.Equal(buf[2047:], buffer2.Bytes()) {
		logError(testName, function, args, startTime, "", "Incorrect read bytes v/s original buffer", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Tests get object ReaderAt interface methods.
func testGetObjectReadAtFunctionalV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(bucketName, objectName)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	buf, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}

	// Save the data
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	defer r.Close()

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(bufSize)+" got "+string(st.Size), err)
		return
	}

	offset := int64(2048)

	// Read directly
	buf2 := make([]byte, 512)
	buf3 := make([]byte, 512)
	buf4 := make([]byte, 512)

	m, err := r.ReadAt(buf2, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf2) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf2))+" got "+string(m), err)
		return
	}
	if !bytes.Equal(buf2, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512
	m, err = r.ReadAt(buf3, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf3) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf3))+" got "+string(m), err)
		return
	}
	if !bytes.Equal(buf3, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}
	offset += 512
	m, err = r.ReadAt(buf4, offset)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAt failed", err)
		return
	}
	if m != len(buf4) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf4))+" got "+string(m), err)
		return
	}
	if !bytes.Equal(buf4, buf[offset:offset+512]) {
		logError(testName, function, args, startTime, "", "Incorrect read between two ReadAt from same offset", err)
		return
	}

	buf5 := make([]byte, bufSize)
	// Read the whole object.
	m, err = r.ReadAt(buf5, 0)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "ReadAt failed", err)
			return
		}
	}
	if m != len(buf5) {
		logError(testName, function, args, startTime, "", "ReadAt read shorter bytes before reaching EOF, expected "+string(len(buf5))+" got "+string(m), err)
		return
	}
	if !bytes.Equal(buf, buf5) {
		logError(testName, function, args, startTime, "", "Incorrect data read in GetObject, than what was previously uploaded", err)
		return
	}

	buf6 := make([]byte, bufSize+1)
	// Read the whole object and beyond.
	_, err = r.ReadAt(buf6, 0)
	if err != nil {
		if err != io.EOF {
			logError(testName, function, args, startTime, "", "ReadAt failed", err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Tests copy object
func testCopyObjectV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, c)

	// Make a new bucket in 'us-east-1' (destination bucket).
	err = c.MakeBucket(context.Background(), bucketName+"-copy", minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName+"-copy", c)

	// Generate 33K of data.
	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	r, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	// Check the various fields of source object against destination object.
	objInfo, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	r.Close()

	// Copy Source
	src := minio.CopySrcOptions{
		Bucket:             bucketName,
		Object:             objectName,
		MatchModifiedSince: time.Date(2014, time.April, 0, 0, 0, 0, 0, time.UTC),
		MatchETag:          objInfo.ETag,
	}
	args["source"] = src

	// Set copy conditions.
	dst := minio.CopyDestOptions{
		Bucket: bucketName + "-copy",
		Object: objectName + "-copy",
	}
	args["destination"] = dst

	// Perform the Copy
	_, err = c.CopyObject(context.Background(), dst, src)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed", err)
		return
	}

	// Source object
	r, err = c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	// Destination object
	readerCopy, err := c.GetObject(context.Background(), bucketName+"-copy", objectName+"-copy", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	// Check the various fields of source object against destination object.
	objInfo, err = r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	objInfoCopy, err := readerCopy.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "Stat failed", err)
		return
	}
	if objInfo.Size != objInfoCopy.Size {
		logError(testName, function, args, startTime, "", "Number of bytes does not match, expected "+string(objInfoCopy.Size)+" got "+string(objInfo.Size), err)
		return
	}

	// Close all the readers.
	r.Close()
	readerCopy.Close()

	// CopyObject again but with wrong conditions
	src = minio.CopySrcOptions{
		Bucket:               bucketName,
		Object:               objectName,
		MatchUnmodifiedSince: time.Date(2014, time.April, 0, 0, 0, 0, 0, time.UTC),
		NoMatchETag:          objInfo.ETag,
	}

	// Perform the Copy which should fail
	_, err = c.CopyObject(context.Background(), dst, src)
	if err == nil {
		logError(testName, function, args, startTime, "", "CopyObject did not fail for invalid conditions", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testComposeObjectErrorCasesWrapper(c *minio.Client) {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ComposeObject(destination, sourceList)"
	args := map[string]interface{}{}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	// Make a new bucket in 'us-east-1' (source bucket).
	err := c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Test that more than 10K source objects cannot be
	// concatenated.
	srcArr := [10001]minio.CopySrcOptions{}
	srcSlice := srcArr[:]
	dst := minio.CopyDestOptions{
		Bucket: bucketName,
		Object: "object",
	}

	args["destination"] = dst
	// Just explain about srcArr in args["sourceList"]
	// to stop having 10,001 null headers logged
	args["sourceList"] = "source array of 10,001 elements"
	if _, err := c.ComposeObject(context.Background(), dst, srcSlice...); err == nil {
		logError(testName, function, args, startTime, "", "Expected error in ComposeObject", err)
		return
	} else if err.Error() != "There must be as least one and up to 10000 source objects." {
		logError(testName, function, args, startTime, "", "Got unexpected error", err)
		return
	}

	// Create a source with invalid offset spec and check that
	// error is returned:
	// 1. Create the source object.
	const badSrcSize = 5 * 1024 * 1024
	buf := bytes.Repeat([]byte("1"), badSrcSize)
	_, err = c.PutObject(context.Background(), bucketName, "badObject", bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	// 2. Set invalid range spec on the object (going beyond
	// object size)
	badSrc := minio.CopySrcOptions{
		Bucket:     bucketName,
		Object:     "badObject",
		MatchRange: true,
		Start:      1,
		End:        badSrcSize,
	}

	// 3. ComposeObject call should fail.
	if _, err := c.ComposeObject(context.Background(), dst, badSrc); err == nil {
		logError(testName, function, args, startTime, "", "ComposeObject expected to fail", err)
		return
	} else if !strings.Contains(err.Error(), "has invalid segment-to-copy") {
		logError(testName, function, args, startTime, "", "Got invalid error", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test expected error cases
func testComposeObjectErrorCasesV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ComposeObject(destination, sourceList)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	testComposeObjectErrorCasesWrapper(c)
}

func testComposeMultipleSources(c *minio.Client) {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ComposeObject(destination, sourceList)"
	args := map[string]interface{}{
		"destination": "",
		"sourceList":  "",
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	// Make a new bucket in 'us-east-1' (source bucket).
	err := c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Upload a small source object
	const srcSize = 1024 * 1024 * 5
	buf := bytes.Repeat([]byte("1"), srcSize)
	_, err = c.PutObject(context.Background(), bucketName, "srcObject", bytes.NewReader(buf), int64(srcSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// We will append 10 copies of the object.
	srcs := []minio.CopySrcOptions{}
	for i := 0; i < 10; i++ {
		srcs = append(srcs, minio.CopySrcOptions{
			Bucket: bucketName,
			Object: "srcObject",
		})
	}

	// make the last part very small
	srcs[9].MatchRange = true

	args["sourceList"] = srcs

	dst := minio.CopyDestOptions{
		Bucket: bucketName,
		Object: "dstObject",
	}
	args["destination"] = dst

	ui, err := c.ComposeObject(context.Background(), dst, srcs...)
	if err != nil {
		logError(testName, function, args, startTime, "", "ComposeObject failed", err)
		return
	}

	if ui.Size != 9*srcSize+1 {
		logError(testName, function, args, startTime, "", "ComposeObject returned unexpected size", err)
		return
	}

	objProps, err := c.StatObject(context.Background(), bucketName, "dstObject", minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}

	if objProps.Size != 9*srcSize+1 {
		logError(testName, function, args, startTime, "", "Size mismatched! Expected "+string(10000*srcSize)+" got "+string(objProps.Size), err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test concatenating multiple 10K objects V2
func testCompose10KSourcesV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ComposeObject(destination, sourceList)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	testComposeMultipleSources(c)
}

func testEncryptedEmptyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader, objectSize, opts)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName
	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	sse := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"object"))

	// 1. create an sse-c encrypted object to copy by uploading
	const srcSize = 0
	var buf []byte // Empty buffer
	args["objectName"] = "object"
	_, err = c.PutObject(context.Background(), bucketName, "object", bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{ServerSideEncryption: sse})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	// 2. Test CopyObject for an empty object
	src := minio.CopySrcOptions{
		Bucket:     bucketName,
		Object:     "object",
		Encryption: sse,
	}

	dst := minio.CopyDestOptions{
		Bucket:     bucketName,
		Object:     "new-object",
		Encryption: sse,
	}

	if _, err = c.CopyObject(context.Background(), dst, src); err != nil {
		function = "CopyObject(dst, src)"
		logError(testName, function, map[string]interface{}{}, startTime, "", "CopyObject failed", err)
		return
	}

	// 3. Test Key rotation
	newSSE := encrypt.DefaultPBKDF([]byte("Don't Panic"), []byte(bucketName+"new-object"))
	src = minio.CopySrcOptions{
		Bucket:     bucketName,
		Object:     "new-object",
		Encryption: sse,
	}

	dst = minio.CopyDestOptions{
		Bucket:     bucketName,
		Object:     "new-object",
		Encryption: newSSE,
	}

	if _, err = c.CopyObject(context.Background(), dst, src); err != nil {
		function = "CopyObject(dst, src)"
		logError(testName, function, map[string]interface{}{}, startTime, "", "CopyObject with key rotation failed", err)
		return
	}

	// 4. Download the object.
	reader, err := c.GetObject(context.Background(), bucketName, "new-object", minio.GetObjectOptions{ServerSideEncryption: newSSE})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	defer reader.Close()

	decBytes, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, map[string]interface{}{}, startTime, "", "ReadAll failed", err)
		return
	}
	if !bytes.Equal(decBytes, buf) {
		logError(testName, function, map[string]interface{}{}, startTime, "", "Downloaded object doesn't match the empty encrypted object", err)
		return
	}

	delete(args, "objectName")
	logSuccess(testName, function, args, startTime)
}

func testEncryptedCopyObjectWrapper(c *minio.Client, bucketName string, sseSrc, sseDst encrypt.ServerSide) {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncNameLoc(2)
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}
	var srcEncryption, dstEncryption encrypt.ServerSide

	// Make a new bucket in 'us-east-1' (source bucket).
	err := c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// 1. create an sse-c encrypted object to copy by uploading
	const srcSize = 1024 * 1024
	buf := bytes.Repeat([]byte("abcde"), srcSize) // gives a buffer of 5MiB
	_, err = c.PutObject(context.Background(), bucketName, "srcObject", bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{
		ServerSideEncryption: sseSrc,
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	if sseSrc != nil && sseSrc.Type() != encrypt.S3 {
		srcEncryption = sseSrc
	}

	// 2. copy object and change encryption key
	src := minio.CopySrcOptions{
		Bucket:     bucketName,
		Object:     "srcObject",
		Encryption: srcEncryption,
	}
	args["source"] = src

	dst := minio.CopyDestOptions{
		Bucket:     bucketName,
		Object:     "dstObject",
		Encryption: sseDst,
	}
	args["destination"] = dst

	_, err = c.CopyObject(context.Background(), dst, src)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed", err)
		return
	}

	if sseDst != nil && sseDst.Type() != encrypt.S3 {
		dstEncryption = sseDst
	}
	// 3. get copied object and check if content is equal
	coreClient := minio.Core{Client: c}
	reader, _, _, err := coreClient.GetObject(context.Background(), bucketName, "dstObject", minio.GetObjectOptions{ServerSideEncryption: dstEncryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	decBytes, err := io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}
	if !bytes.Equal(decBytes, buf) {
		logError(testName, function, args, startTime, "", "Downloaded object mismatched for encrypted object", err)
		return
	}
	reader.Close()

	// Test key rotation for source object in-place.
	var newSSE encrypt.ServerSide
	if sseSrc != nil && sseSrc.Type() == encrypt.SSEC {
		newSSE = encrypt.DefaultPBKDF([]byte("Don't Panic"), []byte(bucketName+"srcObject")) // replace key
	}
	if sseSrc != nil && sseSrc.Type() == encrypt.S3 {
		newSSE = encrypt.NewSSE()
	}
	if newSSE != nil {
		dst = minio.CopyDestOptions{
			Bucket:     bucketName,
			Object:     "srcObject",
			Encryption: newSSE,
		}
		args["destination"] = dst

		_, err = c.CopyObject(context.Background(), dst, src)
		if err != nil {
			logError(testName, function, args, startTime, "", "CopyObject failed", err)
			return
		}

		// Get copied object and check if content is equal
		reader, _, _, err = coreClient.GetObject(context.Background(), bucketName, "srcObject", minio.GetObjectOptions{ServerSideEncryption: newSSE})
		if err != nil {
			logError(testName, function, args, startTime, "", "GetObject failed", err)
			return
		}

		decBytes, err = io.ReadAll(reader)
		if err != nil {
			logError(testName, function, args, startTime, "", "ReadAll failed", err)
			return
		}
		if !bytes.Equal(decBytes, buf) {
			logError(testName, function, args, startTime, "", "Downloaded object mismatched for encrypted object", err)
			return
		}
		reader.Close()

		// Test in-place decryption.
		dst = minio.CopyDestOptions{
			Bucket: bucketName,
			Object: "srcObject",
		}
		args["destination"] = dst

		src = minio.CopySrcOptions{
			Bucket:     bucketName,
			Object:     "srcObject",
			Encryption: newSSE,
		}
		args["source"] = src
		_, err = c.CopyObject(context.Background(), dst, src)
		if err != nil {
			logError(testName, function, args, startTime, "", "CopyObject Key rotation failed", err)
			return
		}
	}

	// Get copied decrypted object and check if content is equal
	reader, _, _, err = coreClient.GetObject(context.Background(), bucketName, "srcObject", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	defer reader.Close()

	decBytes, err = io.ReadAll(reader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}
	if !bytes.Equal(decBytes, buf) {
		logError(testName, function, args, startTime, "", "Downloaded object mismatched for encrypted object", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test encrypted copy object
func testUnencryptedToSSECCopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	sseDst := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"dstObject"))
	testEncryptedCopyObjectWrapper(c, bucketName, nil, sseDst)
}

// Test encrypted copy object
func testUnencryptedToSSES3CopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	var sseSrc encrypt.ServerSide
	sseDst := encrypt.NewSSE()
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

// Test encrypted copy object
func testUnencryptedToUnencryptedCopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	var sseSrc, sseDst encrypt.ServerSide
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

// Test encrypted copy object
func testEncryptedSSECToSSECCopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	sseSrc := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"srcObject"))
	sseDst := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"dstObject"))
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

// Test encrypted copy object
func testEncryptedSSECToSSES3CopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	sseSrc := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"srcObject"))
	sseDst := encrypt.NewSSE()
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

// Test encrypted copy object
func testEncryptedSSECToUnencryptedCopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	sseSrc := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"srcObject"))
	var sseDst encrypt.ServerSide
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

// Test encrypted copy object
func testEncryptedSSES3ToSSECCopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	sseSrc := encrypt.NewSSE()
	sseDst := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"dstObject"))
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

// Test encrypted copy object
func testEncryptedSSES3ToSSES3CopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	sseSrc := encrypt.NewSSE()
	sseDst := encrypt.NewSSE()
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

// Test encrypted copy object
func testEncryptedSSES3ToUnencryptedCopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	sseSrc := encrypt.NewSSE()
	var sseDst encrypt.ServerSide
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

// Test encrypted copy object
func testEncryptedCopyObjectV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}
	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")

	sseSrc := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"srcObject"))
	sseDst := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+"dstObject"))
	testEncryptedCopyObjectWrapper(c, bucketName, sseSrc, sseDst)
}

func testDecryptedCopyObject() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	bucketName, objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-"), "object"
	if err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	encryption := encrypt.DefaultPBKDF([]byte("correct horse battery staple"), []byte(bucketName+objectName))
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(bytes.Repeat([]byte("a"), 1024*1024)), 1024*1024, minio.PutObjectOptions{
		ServerSideEncryption: encryption,
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	src := minio.CopySrcOptions{
		Bucket:     bucketName,
		Object:     objectName,
		Encryption: encrypt.SSECopy(encryption),
	}
	args["source"] = src

	dst := minio.CopyDestOptions{
		Bucket: bucketName,
		Object: "decrypted-" + objectName,
	}
	args["destination"] = dst

	if _, err = c.CopyObject(context.Background(), dst, src); err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed", err)
		return
	}
	if _, err = c.GetObject(context.Background(), bucketName, "decrypted-"+objectName, minio.GetObjectOptions{}); err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}
	logSuccess(testName, function, args, startTime)
}

func testSSECMultipartEncryptedToSSECCopyObjectPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 6MB of data
	buf := bytes.Repeat([]byte("abcdef"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	password := "correct horse battery staple"
	srcencryption := encrypt.DefaultPBKDF([]byte(password), []byte(bucketName+objectName))

	// Upload a 6MB object using multipart mechanism
	uploadID, err := c.NewMultipartUpload(context.Background(), bucketName, objectName, minio.PutObjectOptions{ServerSideEncryption: srcencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	var completeParts []minio.CompletePart

	part, err := c.PutObjectPart(context.Background(), bucketName, objectName, uploadID, 1,
		bytes.NewReader(buf[:5*1024*1024]), 5*1024*1024,
		minio.PutObjectPartOptions{SSE: srcencryption},
	)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectPart call failed", err)
		return
	}
	completeParts = append(completeParts, minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag})

	part, err = c.PutObjectPart(context.Background(), bucketName, objectName, uploadID, 2,
		bytes.NewReader(buf[5*1024*1024:]), 1024*1024,
		minio.PutObjectPartOptions{SSE: srcencryption},
	)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectPart call failed", err)
		return
	}
	completeParts = append(completeParts, minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag})

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), bucketName, objectName, uploadID, completeParts, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{ServerSideEncryption: srcencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"
	dstencryption := encrypt.DefaultPBKDF([]byte(password), []byte(destBucketName+destObjectName))

	uploadID, err = c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	encrypt.SSECopy(srcencryption).Marshal(header)
	dstencryption.Marshal(header)
	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = objInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err = c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (6*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{ServerSideEncryption: dstencryption}
	getOpts.SetRange(0, 6*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 6*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 6MB", err)
		return
	}

	getOpts.SetRange(6*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 6*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:6*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 6MB", err)
		return
	}
	if getBuf[6*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation
func testSSECEncryptedToSSECCopyObjectPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	password := "correct horse battery staple"
	srcencryption := encrypt.DefaultPBKDF([]byte(password), []byte(bucketName+objectName))
	putmetadata := map[string]string{
		"Content-Type": "binary/octet-stream",
	}
	opts := minio.PutObjectOptions{
		UserMetadata:         putmetadata,
		ServerSideEncryption: srcencryption,
	}
	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{ServerSideEncryption: srcencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"
	dstencryption := encrypt.DefaultPBKDF([]byte(password), []byte(destBucketName+destObjectName))

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	encrypt.SSECopy(srcencryption).Marshal(header)
	dstencryption.Marshal(header)
	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{ServerSideEncryption: dstencryption}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation for SSEC encrypted to unencrypted copy
func testSSECEncryptedToUnencryptedCopyPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	password := "correct horse battery staple"
	srcencryption := encrypt.DefaultPBKDF([]byte(password), []byte(bucketName+objectName))

	opts := minio.PutObjectOptions{
		UserMetadata: map[string]string{
			"Content-Type": "binary/octet-stream",
		},
		ServerSideEncryption: srcencryption,
	}
	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{ServerSideEncryption: srcencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"
	var dstencryption encrypt.ServerSide

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	encrypt.SSECopy(srcencryption).Marshal(header)
	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation for SSEC encrypted to SSE-S3 encrypted copy
func testSSECEncryptedToSSES3CopyObjectPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	password := "correct horse battery staple"
	srcencryption := encrypt.DefaultPBKDF([]byte(password), []byte(bucketName+objectName))
	putmetadata := map[string]string{
		"Content-Type": "binary/octet-stream",
	}
	opts := minio.PutObjectOptions{
		UserMetadata:         putmetadata,
		ServerSideEncryption: srcencryption,
	}

	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{ServerSideEncryption: srcencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"
	dstencryption := encrypt.NewSSE()

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	encrypt.SSECopy(srcencryption).Marshal(header)
	dstencryption.Marshal(header)

	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation for unencrypted to SSEC encryption copy part
func testUnencryptedToSSECCopyObjectPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	password := "correct horse battery staple"
	putmetadata := map[string]string{
		"Content-Type": "binary/octet-stream",
	}
	opts := minio.PutObjectOptions{
		UserMetadata: putmetadata,
	}
	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"
	dstencryption := encrypt.DefaultPBKDF([]byte(password), []byte(destBucketName+destObjectName))

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	dstencryption.Marshal(header)
	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{ServerSideEncryption: dstencryption}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation for unencrypted to unencrypted copy
func testUnencryptedToUnencryptedCopyPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	putmetadata := map[string]string{
		"Content-Type": "binary/octet-stream",
	}
	opts := minio.PutObjectOptions{
		UserMetadata: putmetadata,
	}
	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}
	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation for unencrypted to SSE-S3 encrypted copy
func testUnencryptedToSSES3CopyObjectPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	opts := minio.PutObjectOptions{
		UserMetadata: map[string]string{
			"Content-Type": "binary/octet-stream",
		},
	}
	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}
	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"
	dstencryption := encrypt.NewSSE()

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	dstencryption.Marshal(header)

	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation for SSE-S3 to SSEC encryption copy part
func testSSES3EncryptedToSSECCopyObjectPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	password := "correct horse battery staple"
	srcEncryption := encrypt.NewSSE()
	opts := minio.PutObjectOptions{
		UserMetadata: map[string]string{
			"Content-Type": "binary/octet-stream",
		},
		ServerSideEncryption: srcEncryption,
	}
	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{ServerSideEncryption: srcEncryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"
	dstencryption := encrypt.DefaultPBKDF([]byte(password), []byte(destBucketName+destObjectName))

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	dstencryption.Marshal(header)
	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{ServerSideEncryption: dstencryption}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation for unencrypted to unencrypted copy
func testSSES3EncryptedToUnencryptedCopyPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	srcEncryption := encrypt.NewSSE()
	opts := minio.PutObjectOptions{
		UserMetadata: map[string]string{
			"Content-Type": "binary/octet-stream",
		},
		ServerSideEncryption: srcEncryption,
	}
	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}
	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{ServerSideEncryption: srcEncryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

// Test Core CopyObjectPart implementation for unencrypted to SSE-S3 encrypted copy
func testSSES3EncryptedToSSES3CopyObjectPart() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObjectPart(destination, source)"
	args := map[string]interface{}{}

	client, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Instantiate new core client object.
	c := minio.Core{Client: client}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, client)
	// Make a buffer with 5MB of data
	buf := bytes.Repeat([]byte("abcde"), 1024*1024)

	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	srcEncryption := encrypt.NewSSE()
	opts := minio.PutObjectOptions{
		UserMetadata: map[string]string{
			"Content-Type": "binary/octet-stream",
		},
		ServerSideEncryption: srcEncryption,
	}

	uploadInfo, err := c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), "", "", opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}
	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{ServerSideEncryption: srcEncryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}
	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", fmt.Sprintf("Error: number of bytes does not match, want %v, got %v\n", len(buf), st.Size), err)
		return
	}

	destBucketName := bucketName
	destObjectName := objectName + "-dest"
	dstencryption := encrypt.NewSSE()

	uploadID, err := c.NewMultipartUpload(context.Background(), destBucketName, destObjectName, minio.PutObjectOptions{ServerSideEncryption: dstencryption})
	if err != nil {
		logError(testName, function, args, startTime, "", "NewMultipartUpload call failed", err)
		return
	}

	// Content of the destination object will be two copies of
	// `objectName` concatenated, followed by first byte of
	// `objectName`.
	metadata := make(map[string]string)
	header := make(http.Header)
	dstencryption.Marshal(header)

	for k, v := range header {
		metadata[k] = v[0]
	}

	metadata["x-amz-copy-source-if-match"] = uploadInfo.ETag

	// First of three parts
	fstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 1, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Second of three parts
	sndPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 2, 0, -1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Last of three parts
	lstPart, err := c.CopyObjectPart(context.Background(), bucketName, objectName, destBucketName, destObjectName, uploadID, 3, 0, 1, metadata)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObjectPart call failed", err)
		return
	}

	// Complete the multipart upload
	_, err = c.CompleteMultipartUpload(context.Background(), destBucketName, destObjectName, uploadID, []minio.CompletePart{fstPart, sndPart, lstPart}, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "CompleteMultipartUpload call failed", err)
		return
	}

	// Stat the object and check its length matches
	objInfo, err := c.StatObject(context.Background(), destBucketName, destObjectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject call failed", err)
		return
	}

	if objInfo.Size != (5*1024*1024)*2+1 {
		logError(testName, function, args, startTime, "", "Destination object has incorrect size!", err)
		return
	}

	// Now we read the data back
	getOpts := minio.GetObjectOptions{}
	getOpts.SetRange(0, 5*1024*1024-1)
	r, _, _, err := c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf := make([]byte, 5*1024*1024)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf, buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in first 5MB", err)
		return
	}

	getOpts.SetRange(5*1024*1024, 0)
	r, _, _, err = c.GetObject(context.Background(), destBucketName, destObjectName, getOpts)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject call failed", err)
		return
	}
	getBuf = make([]byte, 5*1024*1024+1)
	_, err = readFull(r, getBuf)
	if err != nil {
		logError(testName, function, args, startTime, "", "Read buffer failed", err)
		return
	}
	if !bytes.Equal(getBuf[:5*1024*1024], buf) {
		logError(testName, function, args, startTime, "", "Got unexpected data in second 5MB", err)
		return
	}
	if getBuf[5*1024*1024] != buf[0] {
		logError(testName, function, args, startTime, "", "Got unexpected data in last byte of copied object!", err)
		return
	}

	logSuccess(testName, function, args, startTime)

	// Do not need to remove destBucketName its same as bucketName.
}

func testUserMetadataCopying() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	testUserMetadataCopyingWrapper(c)
}

func testUserMetadataCopyingWrapper(c *minio.Client) {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	// Make a new bucket in 'us-east-1' (source bucket).
	err := c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	fetchMeta := func(object string) (h http.Header) {
		objInfo, err := c.StatObject(context.Background(), bucketName, object, minio.StatObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}
		h = make(http.Header)
		for k, vs := range objInfo.Metadata {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
				h.Add(k, vs[0])
			}
		}
		return h
	}

	// 1. create a client encrypted object to copy by uploading
	const srcSize = 1024 * 1024
	buf := bytes.Repeat([]byte("abcde"), srcSize) // gives a buffer of 5MiB
	metadata := make(http.Header)
	metadata.Set("x-amz-meta-myheader", "myvalue")
	m := make(map[string]string)
	m["x-amz-meta-myheader"] = "myvalue"
	_, err = c.PutObject(context.Background(), bucketName, "srcObject",
		bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{UserMetadata: m})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectWithMetadata failed", err)
		return
	}
	if !reflect.DeepEqual(metadata, fetchMeta("srcObject")) {
		logError(testName, function, args, startTime, "", "Metadata match failed", err)
		return
	}

	// 2. create source
	src := minio.CopySrcOptions{
		Bucket: bucketName,
		Object: "srcObject",
	}

	// 2.1 create destination with metadata set
	dst1 := minio.CopyDestOptions{
		Bucket:          bucketName,
		Object:          "dstObject-1",
		UserMetadata:    map[string]string{"notmyheader": "notmyvalue"},
		ReplaceMetadata: true,
	}

	// 3. Check that copying to an object with metadata set resets
	// the headers on the copy.
	args["source"] = src
	args["destination"] = dst1
	_, err = c.CopyObject(context.Background(), dst1, src)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed", err)
		return
	}

	expectedHeaders := make(http.Header)
	expectedHeaders.Set("x-amz-meta-notmyheader", "notmyvalue")
	if !reflect.DeepEqual(expectedHeaders, fetchMeta("dstObject-1")) {
		logError(testName, function, args, startTime, "", "Metadata match failed", err)
		return
	}

	// 4. create destination with no metadata set and same source
	dst2 := minio.CopyDestOptions{
		Bucket: bucketName,
		Object: "dstObject-2",
	}

	// 5. Check that copying to an object with no metadata set,
	// copies metadata.
	args["source"] = src
	args["destination"] = dst2
	_, err = c.CopyObject(context.Background(), dst2, src)
	if err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed", err)
		return
	}

	expectedHeaders = metadata
	if !reflect.DeepEqual(expectedHeaders, fetchMeta("dstObject-2")) {
		logError(testName, function, args, startTime, "", "Metadata match failed", err)
		return
	}

	// 6. Compose a pair of sources.
	dst3 := minio.CopyDestOptions{
		Bucket:          bucketName,
		Object:          "dstObject-3",
		ReplaceMetadata: true,
	}

	function = "ComposeObject(destination, sources)"
	args["source"] = []minio.CopySrcOptions{src, src}
	args["destination"] = dst3
	_, err = c.ComposeObject(context.Background(), dst3, src, src)
	if err != nil {
		logError(testName, function, args, startTime, "", "ComposeObject failed", err)
		return
	}

	// Check that no headers are copied in this case
	if !reflect.DeepEqual(make(http.Header), fetchMeta("dstObject-3")) {
		logError(testName, function, args, startTime, "", "Metadata match failed", err)
		return
	}

	// 7. Compose a pair of sources with dest user metadata set.
	dst4 := minio.CopyDestOptions{
		Bucket:          bucketName,
		Object:          "dstObject-4",
		UserMetadata:    map[string]string{"notmyheader": "notmyvalue"},
		ReplaceMetadata: true,
	}

	function = "ComposeObject(destination, sources)"
	args["source"] = []minio.CopySrcOptions{src, src}
	args["destination"] = dst4
	_, err = c.ComposeObject(context.Background(), dst4, src, src)
	if err != nil {
		logError(testName, function, args, startTime, "", "ComposeObject failed", err)
		return
	}

	// Check that no headers are copied in this case
	expectedHeaders = make(http.Header)
	expectedHeaders.Set("x-amz-meta-notmyheader", "notmyvalue")
	if !reflect.DeepEqual(expectedHeaders, fetchMeta("dstObject-4")) {
		logError(testName, function, args, startTime, "", "Metadata match failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testUserMetadataCopyingV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "CopyObject(destination, source)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v2 object creation failed", err)
		return
	}

	testUserMetadataCopyingWrapper(c)
}

func testStorageClassMetadataPutObject() {
	// initialize logging params
	startTime := time.Now()
	function := "testStorageClassMetadataPutObject()"
	args := map[string]interface{}{}
	testName := getFuncName()

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")
	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	fetchMeta := func(object string) (h http.Header) {
		objInfo, err := c.StatObject(context.Background(), bucketName, object, minio.StatObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}
		h = make(http.Header)
		for k, vs := range objInfo.Metadata {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-storage-class") {
				for _, v := range vs {
					h.Add(k, v)
				}
			}
		}
		return h
	}

	metadata := make(http.Header)
	metadata.Set("x-amz-storage-class", "REDUCED_REDUNDANCY")

	emptyMetadata := make(http.Header)

	const srcSize = 1024 * 1024
	buf := bytes.Repeat([]byte("abcde"), srcSize) // gives a buffer of 1MiB

	_, err = c.PutObject(context.Background(), bucketName, "srcObjectRRSClass",
		bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{StorageClass: "REDUCED_REDUNDANCY"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Get the returned metadata
	returnedMeta := fetchMeta("srcObjectRRSClass")

	// The response metada should either be equal to metadata (with REDUCED_REDUNDANCY) or emptyMetadata (in case of gateways)
	if !reflect.DeepEqual(metadata, returnedMeta) && !reflect.DeepEqual(emptyMetadata, returnedMeta) {
		logError(testName, function, args, startTime, "", "Metadata match failed", err)
		return
	}

	metadata = make(http.Header)
	metadata.Set("x-amz-storage-class", "STANDARD")

	_, err = c.PutObject(context.Background(), bucketName, "srcObjectSSClass",
		bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{StorageClass: "STANDARD"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	if reflect.DeepEqual(metadata, fetchMeta("srcObjectSSClass")) {
		logError(testName, function, args, startTime, "", "Metadata verification failed, STANDARD storage class should not be a part of response metadata", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testStorageClassInvalidMetadataPutObject() {
	// initialize logging params
	startTime := time.Now()
	function := "testStorageClassInvalidMetadataPutObject()"
	args := map[string]interface{}{}
	testName := getFuncName()

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")
	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	const srcSize = 1024 * 1024
	buf := bytes.Repeat([]byte("abcde"), srcSize) // gives a buffer of 1MiB

	_, err = c.PutObject(context.Background(), bucketName, "srcObjectRRSClass",
		bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{StorageClass: "INVALID_STORAGE_CLASS"})
	if err == nil {
		logError(testName, function, args, startTime, "", "PutObject with invalid storage class passed, was expected to fail", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

func testStorageClassMetadataCopyObject() {
	// initialize logging params
	startTime := time.Now()
	function := "testStorageClassMetadataCopyObject()"
	args := map[string]interface{}{}
	testName := getFuncName()

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v4 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test")
	// Make a new bucket in 'us-east-1' (source bucket).
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	fetchMeta := func(object string) (h http.Header) {
		objInfo, err := c.StatObject(context.Background(), bucketName, object, minio.StatObjectOptions{})
		args["bucket"] = bucketName
		args["object"] = object
		if err != nil {
			logError(testName, function, args, startTime, "", "Stat failed", err)
			return
		}
		h = make(http.Header)
		for k, vs := range objInfo.Metadata {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-storage-class") {
				for _, v := range vs {
					h.Add(k, v)
				}
			}
		}
		return h
	}

	metadata := make(http.Header)
	metadata.Set("x-amz-storage-class", "REDUCED_REDUNDANCY")

	emptyMetadata := make(http.Header)

	const srcSize = 1024 * 1024
	buf := bytes.Repeat([]byte("abcde"), srcSize)

	// Put an object with RRS Storage class
	_, err = c.PutObject(context.Background(), bucketName, "srcObjectRRSClass",
		bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{StorageClass: "REDUCED_REDUNDANCY"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Make server side copy of object uploaded in previous step
	src := minio.CopySrcOptions{
		Bucket: bucketName,
		Object: "srcObjectRRSClass",
	}
	dst := minio.CopyDestOptions{
		Bucket: bucketName,
		Object: "srcObjectRRSClassCopy",
	}
	if _, err = c.CopyObject(context.Background(), dst, src); err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed on RRS", err)
		return
	}

	// Get the returned metadata
	returnedMeta := fetchMeta("srcObjectRRSClassCopy")

	// The response metada should either be equal to metadata (with REDUCED_REDUNDANCY) or emptyMetadata (in case of gateways)
	if !reflect.DeepEqual(metadata, returnedMeta) && !reflect.DeepEqual(emptyMetadata, returnedMeta) {
		logError(testName, function, args, startTime, "", "Metadata match failed", err)
		return
	}

	metadata = make(http.Header)
	metadata.Set("x-amz-storage-class", "STANDARD")

	// Put an object with Standard Storage class
	_, err = c.PutObject(context.Background(), bucketName, "srcObjectSSClass",
		bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{StorageClass: "STANDARD"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Make server side copy of object uploaded in previous step
	src = minio.CopySrcOptions{
		Bucket: bucketName,
		Object: "srcObjectSSClass",
	}
	dst = minio.CopyDestOptions{
		Bucket: bucketName,
		Object: "srcObjectSSClassCopy",
	}
	if _, err = c.CopyObject(context.Background(), dst, src); err != nil {
		logError(testName, function, args, startTime, "", "CopyObject failed on SS", err)
		return
	}
	// Fetch the meta data of copied object
	if reflect.DeepEqual(metadata, fetchMeta("srcObjectSSClassCopy")) {
		logError(testName, function, args, startTime, "", "Metadata verification failed, STANDARD storage class should not be a part of response metadata", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test put object with size -1 byte object.
func testPutObjectNoLengthV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader, size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"size":       -1,
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	objectName := bucketName + "unique"
	args["objectName"] = objectName

	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()
	args["size"] = bufSize

	// Upload an object.
	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, -1, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectWithSize failed", err)
		return
	}

	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}

	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Expected upload object size "+string(bufSize)+" got "+string(st.Size), err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test put objects of unknown size.
func testPutObjectsUnknownV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader,size,opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"size":       "",
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Issues are revealed by trying to upload multiple files of unknown size
	// sequentially (on 4GB machines)
	for i := 1; i <= 4; i++ {
		// Simulate that we could be receiving byte slices of data that we want
		// to upload as a file
		rpipe, wpipe := io.Pipe()
		defer rpipe.Close()
		go func() {
			b := []byte("test")
			wpipe.Write(b)
			wpipe.Close()
		}()

		// Upload the object.
		objectName := fmt.Sprintf("%sunique%d", bucketName, i)
		args["objectName"] = objectName

		ui, err := c.PutObject(context.Background(), bucketName, objectName, rpipe, -1, minio.PutObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "PutObjectStreaming failed", err)
			return
		}

		if ui.Size != 4 {
			logError(testName, function, args, startTime, "", "Expected upload object size "+string(4)+" got "+string(ui.Size), nil)
			return
		}

		st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
		if err != nil {
			logError(testName, function, args, startTime, "", "StatObjectStreaming failed", err)
			return
		}

		if st.Size != int64(4) {
			logError(testName, function, args, startTime, "", "Expected upload object size "+string(4)+" got "+string(st.Size), err)
			return
		}

	}

	logSuccess(testName, function, args, startTime)
}

// Test put object with 0 byte object.
func testPutObject0ByteV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader, size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"size":       0,
		"opts":       "",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	objectName := bucketName + "unique"
	args["objectName"] = objectName
	args["opts"] = minio.PutObjectOptions{}

	// Upload an object.
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader([]byte("")), 0, minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectWithSize failed", err)
		return
	}
	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObjectWithSize failed", err)
		return
	}
	if st.Size != 0 {
		logError(testName, function, args, startTime, "", "Expected upload object size 0 but got "+string(st.Size), err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test put object with 0 byte object with non-US-ASCII characters.
func testPutObjectMetadataNonUSASCIIV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(bucketName, objectName, reader, size, opts)"
	args := map[string]interface{}{
		"bucketName": "",
		"objectName": "",
		"size":       0,
		"opts":       "",
	}
	metadata := map[string]string{
		"test-zh": "你好",
		"test-ja": "こんにちは",
		"test-ko": "안녕하세요",
		"test-ru": "Здравствуй",
		"test-de": "Hallo",
		"test-it": "Ciao",
		"test-pt": "Olá",
		"test-ar": "مرحبا",
		"test-hi": "नमस्ते",
		"test-hu": "Helló",
		"test-ro": "Bună",
		"test-be": "Прывiтанне",
		"test-sl": "Pozdravljen",
		"test-sr": "Здраво",
		"test-bg": "Здравейте",
		"test-uk": "Привіт",
	}
	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	objectName := bucketName + "unique"
	args["objectName"] = objectName
	args["opts"] = minio.PutObjectOptions{}

	// Upload an object.
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader([]byte("")), 0, minio.PutObjectOptions{
		UserMetadata: metadata,
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObjectWithSize failed", err)
		return
	}
	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObjectWithSize failed", err)
		return
	}
	if st.Size != 0 {
		logError(testName, function, args, startTime, "", "Expected upload object size 0 but got "+string(st.Size), err)
		return
	}

	for k, v := range metadata {
		if st.Metadata.Get(http.CanonicalHeaderKey("X-Amz-Meta-"+k)) != v {
			logError(testName, function, args, startTime, "", "Expected upload object metadata "+k+": "+v+" but got "+st.Metadata.Get(http.CanonicalHeaderKey("X-Amz-Meta-"+k)), err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Test expected error cases
func testComposeObjectErrorCases() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ComposeObject(destination, sourceList)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	testComposeObjectErrorCasesWrapper(c)
}

// Test concatenating multiple 10K objects V4
func testCompose10KSources() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ComposeObject(destination, sourceList)"
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	testComposeMultipleSources(c)
}

// Tests comprehensive list of all methods.
func testFunctionalV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "testFunctionalV2()"
	functionAll := ""
	args := map[string]interface{}{}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v2 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	location := "us-east-1"
	// Make a new bucket.
	function = "MakeBucket(bucketName, location)"
	functionAll = "MakeBucket(bucketName, location)"
	args = map[string]interface{}{
		"bucketName": bucketName,
		"location":   location,
	}
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: location})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	// Generate a random file name.
	fileName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	file, err := os.Create(fileName)
	if err != nil {
		logError(testName, function, args, startTime, "", "file create failed", err)
		return
	}
	for i := 0; i < 3; i++ {
		buf := make([]byte, rand.Intn(1<<19))
		_, err = file.Write(buf)
		if err != nil {
			logError(testName, function, args, startTime, "", "file write failed", err)
			return
		}
	}
	file.Close()

	// Verify if bucket exits and you have access.
	var exists bool
	function = "BucketExists(bucketName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
	}
	exists, err = c.BucketExists(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "BucketExists failed", err)
		return
	}
	if !exists {
		logError(testName, function, args, startTime, "", "Could not find existing bucket "+bucketName, err)
		return
	}

	// Make the bucket 'public read/write'.
	function = "SetBucketPolicy(bucketName, bucketPolicy)"
	functionAll += ", " + function

	readWritePolicy := `{"Version": "2012-10-17","Statement": [{"Action": ["s3:ListBucketMultipartUploads", "s3:ListBucket"],"Effect": "Allow","Principal": {"AWS": ["*"]},"Resource": ["arn:aws:s3:::` + bucketName + `"],"Sid": ""}]}`

	args = map[string]interface{}{
		"bucketName":   bucketName,
		"bucketPolicy": readWritePolicy,
	}
	err = c.SetBucketPolicy(context.Background(), bucketName, readWritePolicy)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketPolicy failed", err)
		return
	}

	// List all buckets.
	function = "ListBuckets()"
	functionAll += ", " + function
	args = nil
	buckets, err := c.ListBuckets(context.Background())
	if len(buckets) == 0 {
		logError(testName, function, args, startTime, "", "List buckets cannot be empty", err)
		return
	}
	if err != nil {
		logError(testName, function, args, startTime, "", "ListBuckets failed", err)
		return
	}

	// Verify if previously created bucket is listed in list buckets.
	bucketFound := false
	for _, bucket := range buckets {
		if bucket.Name == bucketName {
			bucketFound = true
		}
	}

	// If bucket not found error out.
	if !bucketFound {
		logError(testName, function, args, startTime, "", "Bucket "+bucketName+"not found", err)
		return
	}

	objectName := bucketName + "unique"

	// Generate data
	buf := bytes.Repeat([]byte("n"), rand.Intn(1<<19))

	args = map[string]interface{}{
		"bucketName":  bucketName,
		"objectName":  objectName,
		"contentType": "",
	}
	_, err = c.PutObject(context.Background(), bucketName, objectName, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	st, err := c.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}
	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", "Expected uploaded object length "+string(len(buf))+" got "+string(st.Size), err)
		return
	}

	objectNameNoLength := objectName + "-nolength"
	args["objectName"] = objectNameNoLength
	_, err = c.PutObject(context.Background(), bucketName, objectNameNoLength, bytes.NewReader(buf), int64(len(buf)), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}
	st, err = c.StatObject(context.Background(), bucketName, objectNameNoLength, minio.StatObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "StatObject failed", err)
		return
	}
	if st.Size != int64(len(buf)) {
		logError(testName, function, args, startTime, "", "Expected uploaded object length "+string(len(buf))+" got "+string(st.Size), err)
		return
	}

	// Instantiate a done channel to close all listing.
	doneCh := make(chan struct{})
	defer close(doneCh)

	objFound := false
	isRecursive := true // Recursive is true.
	function = "ListObjects(bucketName, objectName, isRecursive, doneCh)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName":  bucketName,
		"objectName":  objectName,
		"isRecursive": isRecursive,
	}
	for obj := range c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{UseV1: true, Prefix: objectName, Recursive: isRecursive}) {
		if obj.Key == objectName {
			objFound = true
			break
		}
	}
	if !objFound {
		logError(testName, function, args, startTime, "", "Could not find existing object "+objectName, err)
		return
	}

	incompObjNotFound := true
	function = "ListIncompleteUploads(bucketName, objectName, isRecursive, doneCh)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName":  bucketName,
		"objectName":  objectName,
		"isRecursive": isRecursive,
	}
	for objIncompl := range c.ListIncompleteUploads(context.Background(), bucketName, objectName, isRecursive) {
		if objIncompl.Key != "" {
			incompObjNotFound = false
			break
		}
	}
	if !incompObjNotFound {
		logError(testName, function, args, startTime, "", "Unexpected dangling incomplete upload found", err)
		return
	}

	function = "GetObject(bucketName, objectName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
	}
	newReader, err := c.GetObject(context.Background(), bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	newReadBytes, err := io.ReadAll(newReader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}
	newReader.Close()

	if !bytes.Equal(newReadBytes, buf) {
		logError(testName, function, args, startTime, "", "Bytes mismatch", err)
		return
	}

	function = "FGetObject(bucketName, objectName, fileName)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
		"fileName":   fileName + "-f",
	}
	err = c.FGetObject(context.Background(), bucketName, objectName, fileName+"-f", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FgetObject failed", err)
		return
	}

	// Generate presigned HEAD object url.
	function = "PresignedHeadObject(bucketName, objectName, expires, reqParams)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
		"expires":    3600 * time.Second,
	}
	presignedHeadURL, err := c.PresignedHeadObject(context.Background(), bucketName, objectName, 3600*time.Second, nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedHeadObject failed", err)
		return
	}

	httpClient := &http.Client{
		// Setting a sensible time out of 30secs to wait for response
		// headers. Request is pro-actively canceled after 30secs
		// with no response.
		Timeout:   30 * time.Second,
		Transport: createHTTPTransport(),
	}

	req, err := http.NewRequest(http.MethodHead, presignedHeadURL.String(), nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedHeadObject URL head request failed", err)
		return
	}

	// Verify if presigned url works.
	resp, err := httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedHeadObject URL head request failed", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		logError(testName, function, args, startTime, "", "PresignedHeadObject URL returns status "+string(resp.StatusCode), err)
		return
	}
	if resp.Header.Get("ETag") == "" {
		logError(testName, function, args, startTime, "", "Got empty ETag", err)
		return
	}
	resp.Body.Close()

	// Generate presigned GET object url.
	function = "PresignedGetObject(bucketName, objectName, expires, reqParams)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName,
		"expires":    3600 * time.Second,
	}
	presignedGetURL, err := c.PresignedGetObject(context.Background(), bucketName, objectName, 3600*time.Second, nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject failed", err)
		return
	}

	// Verify if presigned url works.
	req, err = http.NewRequest(http.MethodGet, presignedGetURL.String(), nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject request incorrect", err)
		return
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect", err)
		return
	}

	if resp.StatusCode != http.StatusOK {
		logError(testName, function, args, startTime, "", "PresignedGetObject URL returns status "+string(resp.StatusCode), err)
		return
	}
	newPresignedBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}
	resp.Body.Close()
	if !bytes.Equal(newPresignedBytes, buf) {
		logError(testName, function, args, startTime, "", "Bytes mismatch", err)
		return
	}

	// Set request parameters.
	reqParams := make(url.Values)
	reqParams.Set("response-content-disposition", "attachment; filename=\"test.txt\"")
	// Generate presigned GET object url.
	args["reqParams"] = reqParams
	presignedGetURL, err = c.PresignedGetObject(context.Background(), bucketName, objectName, 3600*time.Second, reqParams)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject failed", err)
		return
	}

	// Verify if presigned url works.
	req, err = http.NewRequest(http.MethodGet, presignedGetURL.String(), nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject request incorrect", err)
		return
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedGetObject response incorrect", err)
		return
	}

	if resp.StatusCode != http.StatusOK {
		logError(testName, function, args, startTime, "", "PresignedGetObject URL returns status "+string(resp.StatusCode), err)
		return
	}
	newPresignedBytes, err = io.ReadAll(resp.Body)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed", err)
		return
	}
	if !bytes.Equal(newPresignedBytes, buf) {
		logError(testName, function, args, startTime, "", "Bytes mismatch", err)
		return
	}
	// Verify content disposition.
	if resp.Header.Get("Content-Disposition") != "attachment; filename=\"test.txt\"" {
		logError(testName, function, args, startTime, "", "wrong Content-Disposition received ", err)
		return
	}

	function = "PresignedPutObject(bucketName, objectName, expires)"
	functionAll += ", " + function
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName + "-presigned",
		"expires":    3600 * time.Second,
	}
	presignedPutURL, err := c.PresignedPutObject(context.Background(), bucketName, objectName+"-presigned", 3600*time.Second)
	if err != nil {
		logError(testName, function, args, startTime, "", "PresignedPutObject failed", err)
		return
	}

	// Generate data more than 32K
	buf = bytes.Repeat([]byte("1"), rand.Intn(1<<10)+32*1024)

	req, err = http.NewRequest(http.MethodPut, presignedPutURL.String(), bytes.NewReader(buf))
	if err != nil {
		logError(testName, function, args, startTime, "", "HTTP request to PresignedPutObject URL failed", err)
		return
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		logError(testName, function, args, startTime, "", "HTTP request to PresignedPutObject URL failed", err)
		return
	}

	// Download the uploaded object to verify
	args = map[string]interface{}{
		"bucketName": bucketName,
		"objectName": objectName + "-presigned",
	}
	newReader, err = c.GetObject(context.Background(), bucketName, objectName+"-presigned", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject of uploaded presigned object failed", err)
		return
	}

	newReadBytes, err = io.ReadAll(newReader)
	if err != nil {
		logError(testName, function, args, startTime, "", "ReadAll failed during get on presigned put object", err)
		return
	}
	newReader.Close()

	if !bytes.Equal(newReadBytes, buf) {
		logError(testName, function, args, startTime, "", "Bytes mismatch on presigned object upload verification", err)
		return
	}

	function = "PresignHeader(method, bucketName, objectName, expires, reqParams, extraHeaders)"
	functionAll += ", " + function
	presignExtraHeaders := map[string][]string{
		"mysecret": {"abcxxx"},
	}
	args = map[string]interface{}{
		"method":       "PUT",
		"bucketName":   bucketName,
		"objectName":   objectName + "-presign-custom",
		"expires":      3600 * time.Second,
		"extraHeaders": presignExtraHeaders,
	}
	_, err = c.PresignHeader(context.Background(), "PUT", bucketName, objectName+"-presign-custom", 3600*time.Second, nil, presignExtraHeaders)
	if err == nil {
		logError(testName, function, args, startTime, "", "Presigned with extra headers succeeded", err)
		return
	}

	os.Remove(fileName)
	os.Remove(fileName + "-f")
	logSuccess(testName, functionAll, args, startTime)
}

// Test get object with GetObject with context
func testGetObjectContext() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(ctx, bucketName, objectName)"
	args := map[string]interface{}{
		"ctx":        "",
		"bucketName": "",
		"objectName": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()
	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	args["ctx"] = ctx
	cancel()

	r, err := c.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed unexpectedly", err)
		return
	}

	if _, err = r.Stat(); err == nil {
		logError(testName, function, args, startTime, "", "GetObject should fail on short timeout", err)
		return
	}
	r.Close()

	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	args["ctx"] = ctx
	defer cancel()

	// Read the data back
	r, err = c.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "object Stat call failed", err)
		return
	}
	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes in stat does not match: want "+string(bufSize)+", got"+string(st.Size), err)
		return
	}
	if err := r.Close(); err != nil {
		logError(testName, function, args, startTime, "", "object Close() call failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test get object with FGetObject with a user provided context
func testFGetObjectContext() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FGetObject(ctx, bucketName, objectName, fileName)"
	args := map[string]interface{}{
		"ctx":        "",
		"bucketName": "",
		"objectName": "",
		"fileName":   "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datafile-1-MB"]
	reader := getDataReader("datafile-1-MB")
	defer reader.Close()
	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	args["ctx"] = ctx
	defer cancel()

	fileName := "tempfile-context"
	args["fileName"] = fileName
	// Read the data back
	err = c.FGetObject(ctx, bucketName, objectName, fileName+"-f", minio.GetObjectOptions{})
	if err == nil {
		logError(testName, function, args, startTime, "", "FGetObject should fail on short timeout", err)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()

	// Read the data back
	err = c.FGetObject(ctx, bucketName, objectName, fileName+"-fcontext", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FGetObject with long timeout failed", err)
		return
	}
	if err = os.Remove(fileName + "-fcontext"); err != nil {
		logError(testName, function, args, startTime, "", "Remove file failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test get object with GetObject with a user provided context
func testGetObjectRanges() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(ctx, bucketName, objectName, fileName)"
	args := map[string]interface{}{
		"ctx":        "",
		"bucketName": "",
		"objectName": "",
		"fileName":   "",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rng := rand.NewSource(time.Now().UnixNano())
	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rng, "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()
	// Save the data
	objectName := randString(60, rng, "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	// Read the data back
	tests := []struct {
		start int64
		end   int64
	}{
		{
			start: 1024,
			end:   1024 + 1<<20,
		},
		{
			start: 20e6,
			end:   20e6 + 10000,
		},
		{
			start: 40e6,
			end:   40e6 + 10000,
		},
		{
			start: 60e6,
			end:   60e6 + 10000,
		},
		{
			start: 80e6,
			end:   80e6 + 10000,
		},
		{
			start: 120e6,
			end:   int64(bufSize),
		},
	}
	for _, test := range tests {
		wantRC := getDataReader("datafile-129-MB")
		io.CopyN(io.Discard, wantRC, test.start)
		want := mustCrcReader(io.LimitReader(wantRC, test.end-test.start+1))
		opts := minio.GetObjectOptions{}
		opts.SetRange(test.start, test.end)
		args["opts"] = fmt.Sprintf("%+v", test)
		obj, err := c.GetObject(ctx, bucketName, objectName, opts)
		if err != nil {
			logError(testName, function, args, startTime, "", "FGetObject with long timeout failed", err)
			return
		}
		err = crcMatches(obj, want)
		if err != nil {
			logError(testName, function, args, startTime, "", fmt.Sprintf("GetObject offset %d -> %d", test.start, test.end), err)
			return
		}
	}

	logSuccess(testName, function, args, startTime)
}

// Test get object ACLs with GetObjectACL with custom provided context
func testGetObjectACLContext() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObjectACL(ctx, bucketName, objectName)"
	args := map[string]interface{}{
		"ctx":        "",
		"bucketName": "",
		"objectName": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datafile-1-MB"]
	reader := getDataReader("datafile-1-MB")
	defer reader.Close()
	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	// Add meta data to add a canned acl
	metaData := map[string]string{
		"X-Amz-Acl": "public-read-write",
	}

	_, err = c.PutObject(context.Background(), bucketName,
		objectName, reader, int64(bufSize),
		minio.PutObjectOptions{
			ContentType:  "binary/octet-stream",
			UserMetadata: metaData,
		})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	args["ctx"] = ctx
	defer cancel()

	// Read the data back
	objectInfo, getObjectACLErr := c.GetObjectACL(ctx, bucketName, objectName)
	if getObjectACLErr != nil {
		logError(testName, function, args, startTime, "", "GetObjectACL failed. ", getObjectACLErr)
		return
	}

	s, ok := objectInfo.Metadata["X-Amz-Acl"]
	if !ok {
		logError(testName, function, args, startTime, "", "GetObjectACL fail unable to find \"X-Amz-Acl\"", nil)
		return
	}

	if len(s) != 1 {
		logError(testName, function, args, startTime, "", "GetObjectACL fail \"X-Amz-Acl\" canned acl expected \"1\" got "+fmt.Sprintf(`"%d"`, len(s)), nil)
		return
	}

	// Do a very limited testing if this is not AWS S3
	if os.Getenv(serverEndpoint) != "s3.amazonaws.com" {
		if s[0] != "private" {
			logError(testName, function, args, startTime, "", "GetObjectACL fail \"X-Amz-Acl\" expected \"private\" but got"+fmt.Sprintf("%q", s[0]), nil)
			return
		}

		logSuccess(testName, function, args, startTime)
		return
	}

	if s[0] != "public-read-write" {
		logError(testName, function, args, startTime, "", "GetObjectACL fail \"X-Amz-Acl\" expected \"public-read-write\" but got"+fmt.Sprintf("%q", s[0]), nil)
		return
	}

	bufSize = dataFileMap["datafile-1-MB"]
	reader2 := getDataReader("datafile-1-MB")
	defer reader2.Close()
	// Save the data
	objectName = randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	// Add meta data to add a canned acl
	metaData = map[string]string{
		"X-Amz-Grant-Read":  "id=fooread@minio.go",
		"X-Amz-Grant-Write": "id=foowrite@minio.go",
	}

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader2, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream", UserMetadata: metaData})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject failed", err)
		return
	}

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	args["ctx"] = ctx
	defer cancel()

	// Read the data back
	objectInfo, getObjectACLErr = c.GetObjectACL(ctx, bucketName, objectName)
	if getObjectACLErr == nil {
		logError(testName, function, args, startTime, "", "GetObjectACL fail", getObjectACLErr)
		return
	}

	if len(objectInfo.Metadata) != 3 {
		logError(testName, function, args, startTime, "", "GetObjectACL fail expected \"3\" ACLs but got "+fmt.Sprintf(`"%d"`, len(objectInfo.Metadata)), nil)
		return
	}

	s, ok = objectInfo.Metadata["X-Amz-Grant-Read"]
	if !ok {
		logError(testName, function, args, startTime, "", "GetObjectACL fail unable to find \"X-Amz-Grant-Read\"", nil)
		return
	}

	if len(s) != 1 {
		logError(testName, function, args, startTime, "", "GetObjectACL fail \"X-Amz-Grant-Read\" acl expected \"1\" got "+fmt.Sprintf(`"%d"`, len(s)), nil)
		return
	}

	if s[0] != "fooread@minio.go" {
		logError(testName, function, args, startTime, "", "GetObjectACL fail \"X-Amz-Grant-Read\" acl expected \"fooread@minio.go\" got "+fmt.Sprintf("%q", s), nil)
		return
	}

	s, ok = objectInfo.Metadata["X-Amz-Grant-Write"]
	if !ok {
		logError(testName, function, args, startTime, "", "GetObjectACL fail unable to find \"X-Amz-Grant-Write\"", nil)
		return
	}

	if len(s) != 1 {
		logError(testName, function, args, startTime, "", "GetObjectACL fail \"X-Amz-Grant-Write\" acl expected \"1\" got "+fmt.Sprintf(`"%d"`, len(s)), nil)
		return
	}

	if s[0] != "foowrite@minio.go" {
		logError(testName, function, args, startTime, "", "GetObjectACL fail \"X-Amz-Grant-Write\" acl expected \"foowrite@minio.go\" got "+fmt.Sprintf("%q", s), nil)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test validates putObject with context to see if request cancellation is honored for V2.
func testPutObjectContextV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "PutObject(ctx, bucketName, objectName, reader, size, opts)"
	args := map[string]interface{}{
		"ctx":        "",
		"bucketName": "",
		"objectName": "",
		"size":       "",
		"opts":       "",
	}
	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Make a new bucket.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, c)
	bufSize := dataFileMap["datatfile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()

	objectName := fmt.Sprintf("test-file-%v", rand.Uint32())
	args["objectName"] = objectName

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	args["ctx"] = ctx
	args["size"] = bufSize
	defer cancel()

	_, err = c.PutObject(ctx, bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject with short timeout failed", err)
		return
	}

	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	args["ctx"] = ctx

	defer cancel()
	reader = getDataReader("datafile-33-kB")
	defer reader.Close()
	_, err = c.PutObject(ctx, bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject with long timeout failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test get object with GetObject with custom context
func testGetObjectContextV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetObject(ctx, bucketName, objectName)"
	args := map[string]interface{}{
		"ctx":        "",
		"bucketName": "",
		"objectName": "",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datafile-33-kB"]
	reader := getDataReader("datafile-33-kB")
	defer reader.Close()
	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	args["ctx"] = ctx
	cancel()

	r, err := c.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject failed unexpectedly", err)
		return
	}
	if _, err = r.Stat(); err == nil {
		logError(testName, function, args, startTime, "", "GetObject should fail on short timeout", err)
		return
	}
	r.Close()

	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()

	// Read the data back
	r, err = c.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "GetObject shouldn't fail on longer timeout", err)
		return
	}

	st, err := r.Stat()
	if err != nil {
		logError(testName, function, args, startTime, "", "object Stat call failed", err)
		return
	}
	if st.Size != int64(bufSize) {
		logError(testName, function, args, startTime, "", "Number of bytes in stat does not match, expected "+string(bufSize)+" got "+string(st.Size), err)
		return
	}
	if err := r.Close(); err != nil {
		logError(testName, function, args, startTime, "", " object Close() call failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test get object with FGetObject with custom context
func testFGetObjectContextV2() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "FGetObject(ctx, bucketName, objectName,fileName)"
	args := map[string]interface{}{
		"ctx":        "",
		"bucketName": "",
		"objectName": "",
		"fileName":   "",
	}

	c, err := NewClient(ClientConfig{CredsV2: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO v2 client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket call failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	bufSize := dataFileMap["datatfile-1-MB"]
	reader := getDataReader("datafile-1-MB")
	defer reader.Close()
	// Save the data
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	args["ctx"] = ctx
	defer cancel()

	fileName := "tempfile-context"
	args["fileName"] = fileName

	// Read the data back
	err = c.FGetObject(ctx, bucketName, objectName, fileName+"-f", minio.GetObjectOptions{})
	if err == nil {
		logError(testName, function, args, startTime, "", "FGetObject should fail on short timeout", err)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()

	// Read the data back
	err = c.FGetObject(ctx, bucketName, objectName, fileName+"-fcontext", minio.GetObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "FGetObject call shouldn't fail on long timeout", err)
		return
	}

	if err = os.Remove(fileName + "-fcontext"); err != nil {
		logError(testName, function, args, startTime, "", "Remove file failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test list object v1 and V2
func testListObjects() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "ListObjects(bucketName, objectPrefix, recursive, doneCh)"
	args := map[string]interface{}{
		"bucketName":   "",
		"objectPrefix": "",
		"recursive":    "true",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	defer cleanupBucket(bucketName, c)

	testObjects := []struct {
		name         string
		storageClass string
	}{
		// Special characters
		{"foo bar", "STANDARD"},
		{"foo-%", "STANDARD"},
		{"random-object-1", "STANDARD"},
		{"random-object-2", "REDUCED_REDUNDANCY"},
	}

	for i, object := range testObjects {
		bufSize := dataFileMap["datafile-33-kB"]
		reader := getDataReader("datafile-33-kB")
		defer reader.Close()
		_, err = c.PutObject(context.Background(), bucketName, object.name, reader, int64(bufSize),
			minio.PutObjectOptions{ContentType: "binary/octet-stream", StorageClass: object.storageClass})
		if err != nil {
			logError(testName, function, args, startTime, "", fmt.Sprintf("PutObject %d call failed", i+1), err)
			return
		}
	}

	testList := func(listFn func(context.Context, string, minio.ListObjectsOptions) <-chan minio.ObjectInfo, bucket string, opts minio.ListObjectsOptions) {
		var objCursor int

		// check for object name and storage-class from listing object result
		for objInfo := range listFn(context.Background(), bucket, opts) {
			if objInfo.Err != nil {
				logError(testName, function, args, startTime, "", "ListObjects failed unexpectedly", err)
				return
			}
			if objInfo.Key != testObjects[objCursor].name {
				logError(testName, function, args, startTime, "", "ListObjects does not return expected object name", err)
				return
			}
			if objInfo.StorageClass != testObjects[objCursor].storageClass {
				// Ignored as Gateways (Azure/GCS etc) wont return storage class
				logIgnored(testName, function, args, startTime, "ListObjects doesn't return expected storage class")
			}
			objCursor++
		}

		if objCursor != len(testObjects) {
			logError(testName, function, args, startTime, "", "ListObjects returned unexpected number of items", errors.New(""))
			return
		}
	}

	testList(c.ListObjects, bucketName, minio.ListObjectsOptions{Recursive: true, UseV1: true})
	testList(c.ListObjects, bucketName, minio.ListObjectsOptions{Recursive: true})
	testList(c.ListObjects, bucketName, minio.ListObjectsOptions{Recursive: true, WithMetadata: true})

	logSuccess(testName, function, args, startTime)
}

// testCors is runnable against S3 itself.
// Just provide the env var MINIO_GO_TEST_BUCKET_CORS with bucket that is public and WILL BE DELETED.
// Recreate this manually each time. Minio-go SDK does not support calling
// SetPublicBucket (put-public-access-block) on S3, otherwise we could script the whole thing.
func testCors() {
	ctx := context.Background()
	startTime := time.Now()
	testName := getFuncName()
	function := "SetBucketCors(bucketName, cors)"
	args := map[string]interface{}{
		"bucketName": "",
		"cors":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Create or reuse a bucket that will get cors settings applied to it and deleted when done
	bucketName := os.Getenv("MINIO_GO_TEST_BUCKET_CORS")
	if bucketName == "" {
		bucketName = randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
		err = c.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
		if err != nil {
			logError(testName, function, args, startTime, "", "MakeBucket failed", err)
			return
		}
	}
	args["bucketName"] = bucketName
	defer cleanupBucket(bucketName, c)

	publicPolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:*"],"Resource":["arn:aws:s3:::` + bucketName + `", "arn:aws:s3:::` + bucketName + `/*"]}]}`
	err = c.SetBucketPolicy(ctx, bucketName, publicPolicy)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketPolicy failed", err)
		return
	}

	// Upload an object for testing.
	objectContents := `some-text-file-contents`
	reader := strings.NewReader(objectContents)
	bufSize := int64(len(objectContents))

	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	_, err = c.PutObject(ctx, bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{ContentType: "binary/octet-stream"})
	if err != nil {
		logError(testName, function, args, startTime, "", "PutObject call failed", err)
		return
	}
	bucketURL := c.EndpointURL().String() + "/" + bucketName + "/"
	objectURL := bucketURL + objectName

	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: createHTTPTransport(),
	}

	errStrAccessForbidden := `<Error><Code>AccessForbidden</Code><Message>CORSResponse: This CORS request is not allowed. This is usually because the evalution of Origin, request method / Access-Control-Request-Method or Access-Control-Request-Headers are not whitelisted`
	testCases := []struct {
		name string

		// Cors rules to apply
		applyCorsRules []cors.Rule

		// Outbound request info
		method  string
		url     string
		headers map[string]string

		// Wanted response
		wantStatus       int
		wantHeaders      map[string]string
		wantBodyContains string
	}{
		{
			name: "apply bucket rules",
			applyCorsRules: []cors.Rule{
				{
					AllowedOrigin: []string{"https"}, // S3 documents 'https' origin, but it does not actually work, see test below.
					AllowedMethod: []string{"PUT"},
					AllowedHeader: []string{"*"},
				},
				{
					AllowedOrigin: []string{"http://www.example1.com"},
					AllowedMethod: []string{"PUT"},
					AllowedHeader: []string{"*"},
					ExposeHeader:  []string{"x-amz-server-side-encryption", "x-amz-request-id"},
					MaxAgeSeconds: 3600,
				},
				{
					AllowedOrigin: []string{"http://www.example2.com"},
					AllowedMethod: []string{"POST"},
					AllowedHeader: []string{"X-My-Special-Header"},
					ExposeHeader:  []string{"X-AMZ-Request-ID"},
				},
				{
					AllowedOrigin: []string{"http://www.example3.com"},
					AllowedMethod: []string{"PUT"},
					AllowedHeader: []string{"X-Example-3-Special-Header"},
					MaxAgeSeconds: 10,
				},
				{
					AllowedOrigin: []string{"*"},
					AllowedMethod: []string{"GET"},
					AllowedHeader: []string{"*"},
					ExposeHeader:  []string{"x-amz-request-id", "X-AMZ-server-side-encryption"},
					MaxAgeSeconds: 3600,
				},
				{
					AllowedOrigin: []string{"http://multiplemethodstest.com"},
					AllowedMethod: []string{"POST", "PUT", "DELETE"},
					AllowedHeader: []string{"x-abc-*", "x-def-*"},
				},
				{
					AllowedOrigin: []string{"http://UPPERCASEEXAMPLE.com"},
					AllowedMethod: []string{"DELETE"},
				},
				{
					AllowedOrigin: []string{"https://*"},
					AllowedMethod: []string{"DELETE"},
					AllowedHeader: []string{"x-abc-*", "x-def-*"},
				},
			},
		},
		{
			name:   "preflight to object url matches example1 rule",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "http://www.example1.com",
				"Access-Control-Request-Method":  "PUT",
				"Access-Control-Request-Headers": "x-another-header,x-could-be-anything",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":      "http://www.example1.com",
				"Access-Control-Allow-Methods":     "PUT",
				"Access-Control-Allow-Headers":     "x-another-header,x-could-be-anything",
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Max-Age":           "3600",
				"Content-Length":                   "0",
				// S3 additionally sets the following headers here, MinIO follows fetch spec and does not:
				// "Access-Control-Expose-Headers":    "",
			},
		},
		{
			name:   "preflight to bucket url matches example1 rule",
			method: http.MethodOptions,
			url:    bucketURL,
			headers: map[string]string{
				"Origin":                         "http://www.example1.com",
				"Access-Control-Request-Method":  "PUT",
				"Access-Control-Request-Headers": "x-another-header,x-could-be-anything",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":      "http://www.example1.com",
				"Access-Control-Allow-Methods":     "PUT",
				"Access-Control-Allow-Headers":     "x-another-header,x-could-be-anything",
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Max-Age":           "3600",
				"Content-Length":                   "0",
			},
		},
		{
			name:   "preflight matches example2 rule with header given",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "http://www.example2.com",
				"Access-Control-Request-Method":  "POST",
				"Access-Control-Request-Headers": "X-My-Special-Header",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":      "http://www.example2.com",
				"Access-Control-Allow-Methods":     "POST",
				"Access-Control-Allow-Headers":     "x-my-special-header",
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Max-Age":           "",
				"Content-Length":                   "0",
			},
		},
		{
			name:   "preflight matches example2 rule with no header given",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://www.example2.com",
				"Access-Control-Request-Method": "POST",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":      "http://www.example2.com",
				"Access-Control-Allow-Methods":     "POST",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Max-Age":           "",
				"Content-Length":                   "0",
			},
		},
		{
			name:   "preflight matches wildcard origin rule",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "http://www.couldbeanything.com",
				"Access-Control-Request-Method":  "GET",
				"Access-Control-Request-Headers": "x-custom-header,x-other-custom-header",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":      "*",
				"Access-Control-Allow-Methods":     "GET",
				"Access-Control-Allow-Headers":     "x-custom-header,x-other-custom-header",
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Max-Age":           "3600",
				"Content-Length":                   "0",
			},
		},
		{
			name:   "preflight does not match any rule",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://www.couldbeanything.com",
				"Access-Control-Request-Method": "DELETE",
			},
			wantStatus:       http.StatusForbidden,
			wantBodyContains: errStrAccessForbidden,
		},
		{
			name:   "preflight does not match example1 rule because of method",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://www.example1.com",
				"Access-Control-Request-Method": "POST",
			},
			wantStatus:       http.StatusForbidden,
			wantBodyContains: errStrAccessForbidden,
		},
		{
			name:   "s3 processes cors rules even when request is not preflight if cors headers present test get",
			method: http.MethodGet,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "http://www.example1.com",
				"Access-Control-Request-Headers": "x-another-header,x-could-be-anything",
				"Access-Control-Request-Method":  "PUT",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Origin":      "http://www.example1.com",
				"Access-Control-Expose-Headers":    "x-amz-server-side-encryption,x-amz-request-id",
				// S3 additionally sets the following headers here, MinIO follows fetch spec and does not:
				// "Access-Control-Allow-Headers":     "x-another-header,x-could-be-anything",
				// "Access-Control-Allow-Methods":     "PUT",
				// "Access-Control-Max-Age":           "3600",
			},
		},
		{
			name:   "s3 processes cors rules even when request is not preflight if cors headers present test put",
			method: http.MethodPut,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://www.example1.com",
				"Access-Control-Request-Method": "GET",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Allow-Origin":      "*",
				"Access-Control-Expose-Headers":    "x-amz-request-id,x-amz-server-side-encryption",
				// S3 additionally sets the following headers here, MinIO follows fetch spec and does not:
				// "Access-Control-Allow-Headers":     "x-another-header,x-could-be-anything",
				// "Access-Control-Allow-Methods":     "PUT",
				// "Access-Control-Max-Age":           "3600",
			},
		},
		{
			name:   "s3 processes cors rules even when request is not preflight but there is no rule match",
			method: http.MethodGet,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "http://www.example1.com",
				"Access-Control-Request-Headers": "x-another-header,x-could-be-anything",
				"Access-Control-Request-Method":  "DELETE",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Methods":     "",
				"Access-Control-Allow-Origin":      "",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:   "get request matches wildcard origin rule and returns cors headers",
			method: http.MethodGet,
			url:    objectURL,
			headers: map[string]string{
				"Origin": "http://www.example1.com",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Allow-Origin":      "*",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "x-amz-request-id,X-AMZ-server-side-encryption",
				// S3 returns the following headers, MinIO follows fetch spec and does not:
				// "Access-Control-Max-Age":           "3600",
				// "Access-Control-Allow-Methods":     "GET",
			},
		},
		{
			name:   "head request does not match rule and returns no cors headers",
			method: http.MethodHead,
			url:    objectURL,
			headers: map[string]string{
				"Origin": "http://www.nomatchingdomainfound.com",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Allow-Methods":     "",
				"Access-Control-Allow-Origin":      "",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:   "put request with origin does not match rule and returns no cors headers",
			method: http.MethodPut,
			url:    objectURL,
			headers: map[string]string{
				"Origin": "http://www.nomatchingdomainfound.com",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Allow-Methods":     "",
				"Access-Control-Allow-Origin":      "",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:       "put request with no origin does not match rule and returns no cors headers",
			method:     http.MethodPut,
			url:        objectURL,
			headers:    map[string]string{},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Allow-Methods":     "",
				"Access-Control-Allow-Origin":      "",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:   "preflight for delete request with wildcard origin does not match",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://www.notsecureexample.com",
				"Access-Control-Request-Method": "DELETE",
			},
			wantStatus:       http.StatusForbidden,
			wantBodyContains: errStrAccessForbidden,
		},
		{
			name:   "preflight for delete request with wildcard https origin matches secureexample",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "https://www.secureexample.com",
				"Access-Control-Request-Method": "DELETE",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Methods":     "DELETE",
				"Access-Control-Allow-Origin":      "https://www.secureexample.com",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:   "preflight for delete request matches secureexample with wildcard https origin and request headers",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "https://www.secureexample.com",
				"Access-Control-Request-Method":  "DELETE",
				"Access-Control-Request-Headers": "x-abc-1,x-abc-second,x-def-1",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Methods":     "DELETE",
				"Access-Control-Allow-Origin":      "https://www.secureexample.com",
				"Access-Control-Allow-Headers":     "x-abc-1,x-abc-second,x-def-1",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:   "preflight for delete request matches secureexample rejected because request header does not match",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "https://www.secureexample.com",
				"Access-Control-Request-Method":  "DELETE",
				"Access-Control-Request-Headers": "x-abc-1,x-abc-second,x-def-1,x-does-not-match",
			},
			wantStatus:       http.StatusForbidden,
			wantBodyContains: errStrAccessForbidden,
		},
		{
			name:   "preflight with https origin is documented by s3 as matching but it does not match",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "https://www.securebutdoesnotmatch.com",
				"Access-Control-Request-Method": "PUT",
			},
			wantStatus:       http.StatusForbidden,
			wantBodyContains: errStrAccessForbidden,
		},
		{
			name:       "put no origin no match returns no cors headers",
			method:     http.MethodPut,
			url:        objectURL,
			headers:    map[string]string{},
			wantStatus: http.StatusOK,

			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Allow-Methods":     "",
				"Access-Control-Allow-Origin":      "",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:   "put with origin match example1 returns cors headers",
			method: http.MethodPut,
			url:    objectURL,
			headers: map[string]string{
				"Origin": "http://www.example1.com",
			},
			wantStatus: http.StatusOK,

			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Origin":      "http://www.example1.com",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "x-amz-server-side-encryption,x-amz-request-id",
				// S3 returns the following headers, MinIO follows fetch spec and does not:
				// "Access-Control-Max-Age":           "3600",
				// "Access-Control-Allow-Methods":     "PUT",
			},
		},
		{
			name:   "put with origin and header match example1 returns cors headers",
			method: http.MethodPut,
			url:    objectURL,
			headers: map[string]string{
				"Origin":              "http://www.example1.com",
				"x-could-be-anything": "myvalue",
			},
			wantStatus: http.StatusOK,

			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Origin":      "http://www.example1.com",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "x-amz-server-side-encryption,x-amz-request-id",
				// S3 returns the following headers, MinIO follows fetch spec and does not:
				// "Access-Control-Max-Age":           "3600",
				// "Access-Control-Allow-Methods":     "PUT",
			},
		},
		{
			name:   "put no match found returns no cors headers",
			method: http.MethodPut,
			url:    objectURL,
			headers: map[string]string{
				"Origin": "http://www.unmatchingdomain.com",
			},
			wantStatus: http.StatusOK,

			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "",
				"Access-Control-Allow-Methods":     "",
				"Access-Control-Allow-Origin":      "",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:   "put with origin match example3 returns cors headers",
			method: http.MethodPut,
			url:    objectURL,
			headers: map[string]string{
				"Origin":              "http://www.example3.com",
				"X-My-Special-Header": "myvalue",
			},
			wantStatus: http.StatusOK,

			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Origin":      "http://www.example3.com",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "",
				// S3 returns the following headers, MinIO follows fetch spec and does not:
				// "Access-Control-Max-Age":           "10",
				// "Access-Control-Allow-Methods":     "PUT",
			},
		},
		{
			name:   "preflight matches example1 rule headers case is incorrect",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://www.example1.com",
				"Access-Control-Request-Method": "PUT",
				// Fetch standard guarantees that these are sent lowercase, here we test what happens when they are not.
				"Access-Control-Request-Headers": "X-Another-Header,X-Could-Be-Anything",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":      "http://www.example1.com",
				"Access-Control-Allow-Methods":     "PUT",
				"Access-Control-Allow-Headers":     "x-another-header,x-could-be-anything",
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Max-Age":           "3600",
				"Content-Length":                   "0",
				// S3 returns the following headers, MinIO follows fetch spec and does not:
				// "Access-Control-Expose-Headers":    "x-amz-server-side-encryption,x-amz-request-id",
			},
		},
		{
			name:   "preflight matches example1 rule headers are not sorted",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://www.example1.com",
				"Access-Control-Request-Method": "PUT",
				// Fetch standard guarantees that these are sorted, test what happens when they are not.
				"Access-Control-Request-Headers": "a-customer-header,b-should-be-last",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":      "http://www.example1.com",
				"Access-Control-Allow-Methods":     "PUT",
				"Access-Control-Allow-Headers":     "a-customer-header,b-should-be-last",
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Max-Age":           "3600",
				"Content-Length":                   "0",
				// S3 returns the following headers, MinIO follows fetch spec and does not:
				// "Access-Control-Expose-Headers":    "x-amz-server-side-encryption,x-amz-request-id",
			},
		},
		{
			name:   "preflight with case sensitivity in origin matches uppercase",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://UPPERCASEEXAMPLE.com",
				"Access-Control-Request-Method": "DELETE",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Methods":     "DELETE",
				"Access-Control-Allow-Origin":      "http://UPPERCASEEXAMPLE.com",
				"Access-Control-Allow-Headers":     "",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
			},
		},
		{
			name:   "preflight with case sensitivity in origin does not match when lowercase",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                        "http://uppercaseexample.com",
				"Access-Control-Request-Method": "DELETE",
			},
			wantStatus:       http.StatusForbidden,
			wantBodyContains: errStrAccessForbidden,
		},
		{
			name:   "preflight match upper case with unknown header but no header restrictions",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "http://UPPERCASEEXAMPLE.com",
				"Access-Control-Request-Method":  "DELETE",
				"Access-Control-Request-Headers": "x-unknown-1",
			},
			wantStatus:       http.StatusForbidden,
			wantBodyContains: errStrAccessForbidden,
		},
		{
			name:   "preflight for delete request matches multiplemethodstest.com origin and request headers",
			method: http.MethodOptions,
			url:    objectURL,
			headers: map[string]string{
				"Origin":                         "http://multiplemethodstest.com",
				"Access-Control-Request-Method":  "DELETE",
				"Access-Control-Request-Headers": "x-abc-1",
			},
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Origin":      "http://multiplemethodstest.com",
				"Access-Control-Allow-Headers":     "x-abc-1",
				"Access-Control-Expose-Headers":    "",
				"Access-Control-Max-Age":           "",
				// S3 returns POST, PUT, DELETE here, MinIO does not as spec does not require it.
				// "Access-Control-Allow-Methods":     "DELETE",
			},
		},
		{
			name:   "delete request goes ahead because cors is only for browsers and does not block on the server side",
			method: http.MethodDelete,
			url:    objectURL,
			headers: map[string]string{
				"Origin": "http://www.justrandom.com",
			},
			wantStatus: http.StatusNoContent,
		},
	}

	for i, test := range testCases {
		testName := fmt.Sprintf("%s_%d_%s", testName, i+1, strings.ReplaceAll(test.name, " ", "_"))

		// Apply the CORS rules
		if test.applyCorsRules != nil {
			corsConfig := &cors.Config{
				CORSRules: test.applyCorsRules,
			}
			err = c.SetBucketCors(ctx, bucketName, corsConfig)
			if err != nil {
				logError(testName, function, args, startTime, "", "SetBucketCors failed to apply", err)
				return
			}
		}

		// Make request
		if test.method != "" && test.url != "" {
			req, err := http.NewRequestWithContext(ctx, test.method, test.url, nil)
			if err != nil {
				logError(testName, function, args, startTime, "", "HTTP request creation failed", err)
				return
			}
			req.Header.Set("User-Agent", "MinIO-go-FunctionalTest/"+appVersion)

			for k, v := range test.headers {
				req.Header.Set(k, v)
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				logError(testName, function, args, startTime, "", "HTTP request failed", err)
				return
			}
			defer resp.Body.Close()

			// Check returned status code
			if resp.StatusCode != test.wantStatus {
				errStr := fmt.Sprintf(" incorrect status code in response, want: %d, got: %d", test.wantStatus, resp.StatusCode)
				logError(testName, function, args, startTime, "", errStr, nil)
				return
			}

			// Check returned body
			if test.wantBodyContains != "" {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					logError(testName, function, args, startTime, "", "Failed to read response body", err)
					return
				}
				if !strings.Contains(string(body), test.wantBodyContains) {
					errStr := fmt.Sprintf(" incorrect body in response, want: %s, in got: %s", test.wantBodyContains, string(body))
					logError(testName, function, args, startTime, "", errStr, nil)
					return
				}
			}

			// Check returned response headers
			for k, v := range test.wantHeaders {
				gotVal := resp.Header.Get(k)
				if k == "Access-Control-Expose-Headers" {
					// MinIO returns this in canonical form, S3 does not.
					gotVal = strings.ToLower(gotVal)
					v = strings.ToLower(v)
				}
				// Remove all spaces, S3 adds spaces after CSV values in headers, MinIO does not.
				gotVal = strings.ReplaceAll(gotVal, " ", "")
				if gotVal != v {
					errStr := fmt.Sprintf(" incorrect header in response, want: %s: '%s', got: '%s'", k, v, gotVal)
					logError(testName, function, args, startTime, "", errStr, nil)
					return
				}
			}
		}
		logSuccess(testName, function, args, startTime)
	}
	logSuccess(testName, function, args, startTime)
}

func testCorsSetGetDelete() {
	ctx := context.Background()
	startTime := time.Now()
	testName := getFuncName()
	function := "SetBucketCors(bucketName, cors)"
	args := map[string]interface{}{
		"bucketName": "",
		"cors":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: "us-east-1"})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}
	defer cleanupBucket(bucketName, c)

	// Set the CORS rules on the new bucket
	corsRules := []cors.Rule{
		{
			AllowedOrigin: []string{"http://www.example1.com"},
			AllowedMethod: []string{"PUT"},
			AllowedHeader: []string{"*"},
		},
		{
			AllowedOrigin: []string{"http://www.example2.com"},
			AllowedMethod: []string{"POST"},
			AllowedHeader: []string{"X-My-Special-Header"},
		},
		{
			AllowedOrigin: []string{"*"},
			AllowedMethod: []string{"GET"},
			AllowedHeader: []string{"*"},
		},
	}
	corsConfig := cors.NewConfig(corsRules)
	err = c.SetBucketCors(ctx, bucketName, corsConfig)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketCors failed to apply", err)
		return
	}

	// Get the rules and check they match what we set
	gotCorsConfig, err := c.GetBucketCors(ctx, bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketCors failed", err)
		return
	}
	if !reflect.DeepEqual(corsConfig, gotCorsConfig) {
		msg := fmt.Sprintf("GetBucketCors returned unexpected rules, expected: %+v, got: %+v", corsConfig, gotCorsConfig)
		logError(testName, function, args, startTime, "", msg, nil)
		return
	}

	// Delete the rules
	err = c.SetBucketCors(ctx, bucketName, nil)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketCors failed to delete", err)
		return
	}

	// Get the rules and check they are now empty
	gotCorsConfig, err = c.GetBucketCors(ctx, bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketCors failed", err)
		return
	}
	if gotCorsConfig != nil {
		logError(testName, function, args, startTime, "", "GetBucketCors returned unexpected rules", nil)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test deleting multiple objects with object retention set in Governance mode
func testRemoveObjects() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "RemoveObjects(bucketName, objectsCh, opts)"
	args := map[string]interface{}{
		"bucketName":   "",
		"objectPrefix": "",
		"recursive":    "true",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Error uploading object", err)
		return
	}

	// Replace with smaller...
	bufSize = dataFileMap["datafile-10-kB"]
	reader = getDataReader("datafile-10-kB")
	defer reader.Close()

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Error uploading object", err)
	}

	t := time.Date(2030, time.April, 25, 14, 0, 0, 0, time.UTC)
	m := minio.RetentionMode(minio.Governance)
	opts := minio.PutObjectRetentionOptions{
		GovernanceBypass: false,
		RetainUntilDate:  &t,
		Mode:             &m,
	}
	err = c.PutObjectRetention(context.Background(), bucketName, objectName, opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "Error setting retention", err)
		return
	}

	objectsCh := make(chan minio.ObjectInfo)
	// Send object names that are needed to be removed to objectsCh
	go func() {
		defer close(objectsCh)
		// List all objects from a bucket-name with a matching prefix.
		for object := range c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{UseV1: true, Recursive: true}) {
			if object.Err != nil {
				logError(testName, function, args, startTime, "", "Error listing objects", object.Err)
				return
			}
			objectsCh <- object
		}
	}()

	for rErr := range c.RemoveObjects(context.Background(), bucketName, objectsCh, minio.RemoveObjectsOptions{}) {
		// Error is expected here because Retention is set on the object
		// and RemoveObjects is called without Bypass Governance
		if rErr.Err == nil {
			logError(testName, function, args, startTime, "", "Expected error during deletion", nil)
			return
		}
	}

	objectsCh1 := make(chan minio.ObjectInfo)

	// Send object names that are needed to be removed to objectsCh
	go func() {
		defer close(objectsCh1)
		// List all objects from a bucket-name with a matching prefix.
		for object := range c.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{UseV1: true, Recursive: true}) {
			if object.Err != nil {
				logError(testName, function, args, startTime, "", "Error listing objects", object.Err)
				return
			}
			objectsCh1 <- object
		}
	}()

	opts1 := minio.RemoveObjectsOptions{
		GovernanceBypass: true,
	}

	for rErr := range c.RemoveObjects(context.Background(), bucketName, objectsCh1, opts1) {
		// Error is not expected here because Retention is set on the object
		// and RemoveObjects is called with Bypass Governance
		logError(testName, function, args, startTime, "", "Error detected during deletion", rErr.Err)
		return
	}

	// Delete all objects and buckets
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test deleting multiple objects with object retention set in Governance mode, via iterators
func testRemoveObjectsIter() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "RemoveObjects(bucketName, objectsCh, opts)"
	args := map[string]interface{}{
		"bucketName":   "",
		"objectPrefix": "",
		"recursive":    "true",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName
	objectName := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	args["objectName"] = objectName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	bufSize := dataFileMap["datafile-129-MB"]
	reader := getDataReader("datafile-129-MB")
	defer reader.Close()

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Error uploading object", err)
		return
	}

	// Replace with smaller...
	bufSize = dataFileMap["datafile-10-kB"]
	reader = getDataReader("datafile-10-kB")
	defer reader.Close()

	_, err = c.PutObject(context.Background(), bucketName, objectName, reader, int64(bufSize), minio.PutObjectOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Error uploading object", err)
	}

	t := time.Date(2030, time.April, 25, 14, 0, 0, 0, time.UTC)
	m := minio.RetentionMode(minio.Governance)
	opts := minio.PutObjectRetentionOptions{
		GovernanceBypass: false,
		RetainUntilDate:  &t,
		Mode:             &m,
	}
	err = c.PutObjectRetention(context.Background(), bucketName, objectName, opts)
	if err != nil {
		logError(testName, function, args, startTime, "", "Error setting retention", err)
		return
	}

	objectsIter := c.ListObjectsIter(context.Background(), bucketName, minio.ListObjectsOptions{
		WithVersions: true,
		Recursive:    true,
	})
	results, err := c.RemoveObjectsWithIter(context.Background(), bucketName, objectsIter, minio.RemoveObjectsOptions{})
	if err != nil {
		logError(testName, function, args, startTime, "", "Error sending delete request", err)
		return
	}
	for result := range results {
		if result.Err != nil {
			// Error is expected here because Retention is set on the object
			// and RemoveObjects is called without Bypass Governance
			break
		}
		logError(testName, function, args, startTime, "", "Expected error during deletion", nil)
		return
	}

	objectsIter = c.ListObjectsIter(context.Background(), bucketName, minio.ListObjectsOptions{UseV1: true, Recursive: true})
	results, err = c.RemoveObjectsWithIter(context.Background(), bucketName, objectsIter, minio.RemoveObjectsOptions{
		GovernanceBypass: true,
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "Error sending delete request", err)
		return
	}
	for result := range results {
		if result.Err != nil {
			// Error is not expected here because Retention is set on the object
			// and RemoveObjects is called with Bypass Governance
			logError(testName, function, args, startTime, "", "Error detected during deletion", result.Err)
			return
		}
	}

	// Delete all objects and buckets
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test get bucket tags
func testGetBucketTagging() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "GetBucketTagging(bucketName)"
	args := map[string]interface{}{
		"bucketName": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	_, err = c.GetBucketTagging(context.Background(), bucketName)
	if minio.ToErrorResponse(err).Code != minio.NoSuchTagSet {
		logError(testName, function, args, startTime, "", "Invalid error from server failed", err)
		return
	}

	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test setting tags for bucket
func testSetBucketTagging() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "SetBucketTagging(bucketName, tags)"
	args := map[string]interface{}{
		"bucketName": "",
		"tags":       "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	_, err = c.GetBucketTagging(context.Background(), bucketName)
	if minio.ToErrorResponse(err).Code != minio.NoSuchTagSet {
		logError(testName, function, args, startTime, "", "Invalid error from server", err)
		return
	}

	tag := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	expectedValue := randString(60, rand.NewSource(time.Now().UnixNano()), "")

	t, err := tags.MapToBucketTags(map[string]string{
		tag: expectedValue,
	})
	args["tags"] = t.String()
	if err != nil {
		logError(testName, function, args, startTime, "", "tags.MapToBucketTags failed", err)
		return
	}

	err = c.SetBucketTagging(context.Background(), bucketName, t)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketTagging failed", err)
		return
	}

	tagging, err := c.GetBucketTagging(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketTagging failed", err)
		return
	}

	if tagging.ToMap()[tag] != expectedValue {
		msg := fmt.Sprintf("Tag %s; got value %s; wanted %s", tag, tagging.ToMap()[tag], expectedValue)
		logError(testName, function, args, startTime, "", msg, err)
		return
	}

	// Delete all objects and buckets
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Test removing bucket tags
func testRemoveBucketTagging() {
	// initialize logging params
	startTime := time.Now()
	testName := getFuncName()
	function := "RemoveBucketTagging(bucketName)"
	args := map[string]interface{}{
		"bucketName": "",
	}

	c, err := NewClient(ClientConfig{})
	if err != nil {
		logError(testName, function, args, startTime, "", "MinIO client v4 object creation failed", err)
		return
	}

	// Generate a new random bucket name.
	bucketName := randString(60, rand.NewSource(time.Now().UnixNano()), "minio-go-test-")
	args["bucketName"] = bucketName

	// Make a new bucket.
	err = c.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{Region: "us-east-1", ObjectLocking: true})
	if err != nil {
		logError(testName, function, args, startTime, "", "MakeBucket failed", err)
		return
	}

	_, err = c.GetBucketTagging(context.Background(), bucketName)
	if minio.ToErrorResponse(err).Code != minio.NoSuchTagSet {
		logError(testName, function, args, startTime, "", "Invalid error from server", err)
		return
	}

	tag := randString(60, rand.NewSource(time.Now().UnixNano()), "")
	expectedValue := randString(60, rand.NewSource(time.Now().UnixNano()), "")

	t, err := tags.MapToBucketTags(map[string]string{
		tag: expectedValue,
	})
	if err != nil {
		logError(testName, function, args, startTime, "", "tags.MapToBucketTags failed", err)
		return
	}

	err = c.SetBucketTagging(context.Background(), bucketName, t)
	if err != nil {
		logError(testName, function, args, startTime, "", "SetBucketTagging failed", err)
		return
	}

	tagging, err := c.GetBucketTagging(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "GetBucketTagging failed", err)
		return
	}

	if tagging.ToMap()[tag] != expectedValue {
		msg := fmt.Sprintf("Tag %s; got value %s; wanted %s", tag, tagging.ToMap()[tag], expectedValue)
		logError(testName, function, args, startTime, "", msg, err)
		return
	}

	err = c.RemoveBucketTagging(context.Background(), bucketName)
	if err != nil {
		logError(testName, function, args, startTime, "", "RemoveBucketTagging failed", err)
		return
	}

	_, err = c.GetBucketTagging(context.Background(), bucketName)
	if minio.ToErrorResponse(err).Code != minio.NoSuchTagSet {
		logError(testName, function, args, startTime, "", "Invalid error from server", err)
		return
	}

	// Delete all objects and buckets
	if err = cleanupVersionedBucket(bucketName, c); err != nil {
		logError(testName, function, args, startTime, "", "CleanupBucket failed", err)
		return
	}

	logSuccess(testName, function, args, startTime)
}

// Convert string to bool and always return false if any error
func mustParseBool(str string) bool {
	b, err := strconv.ParseBool(str)
	if err != nil {
		return false
	}
	return b
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(
		os.Stdout,
		&slog.HandlerOptions{
			Level: slog.LevelInfo,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.MessageKey || a.Value.String() == "" {
					return slog.Attr{}
				}

				return a
			},
		},
	)))

	tls := mustParseBool(os.Getenv(enableHTTPS))
	kms := mustParseBool(os.Getenv(enableKMS))
	if os.Getenv(enableKMS) == "" {
		// Default to KMS tests.
		kms = true
	}

	// execute tests
	if isFullMode() {
		testCorsSetGetDelete()
		testCors()
		testListMultipartUpload()
		testGetObjectAttributes()
		testGetObjectAttributesErrorCases()
		testMakeBucketErrorV2()
		testGetObjectClosedTwiceV2()
		testFPutObjectV2()
		testMakeBucketRegionsV2()
		testGetObjectReadSeekFunctionalV2()
		testGetObjectReadAtFunctionalV2()
		testGetObjectRanges()
		testCopyObjectV2()
		testFunctionalV2()
		testComposeObjectErrorCasesV2()
		testCompose10KSourcesV2()
		testUserMetadataCopyingV2()
		testPutObjectWithChecksums()
		testPutObjectWithTrailingChecksums()
		testPutMultipartObjectWithChecksums(false)
		testPutMultipartObjectWithChecksums(true)
		testPutObject0ByteV2()
		testPutObjectMetadataNonUSASCIIV2()
		testPutObjectNoLengthV2()
		testPutObjectsUnknownV2()
		testGetObjectContextV2()
		testFPutObjectContextV2()
		testFGetObjectContextV2()
		testPutObjectContextV2()
		testPutObjectWithVersioning()
		testMakeBucketError()
		testMakeBucketRegions()
		testPutObjectWithMetadata()
		testPutObjectReadAt()
		testPutObjectStreaming()
		testGetObjectSeekEnd()
		testGetObjectClosedTwice()
		testGetObjectS3Zip()
		testRemoveMultipleObjects()
		testRemoveMultipleObjectsWithResult()
		testRemoveMultipleObjectsIter()
		testFPutObjectMultipart()
		testFPutObject()
		testGetObjectReadSeekFunctional()
		testGetObjectReadAtFunctional()
		testGetObjectReadAtWhenEOFWasReached()
		testPresignedPostPolicy()
		testPresignedPostPolicyWrongFile()
		testPresignedPostPolicyEmptyFileName()
		testCopyObject()
		testComposeObjectErrorCases()
		testCompose10KSources()
		testUserMetadataCopying()
		testBucketNotification()
		testFunctional()
		testGetObjectModified()
		testPutObjectUploadSeekedObject()
		testGetObjectContext()
		testFPutObjectContext()
		testFGetObjectContext()
		testGetObjectACLContext()
		testPutObjectContext()
		testStorageClassMetadataPutObject()
		testStorageClassInvalidMetadataPutObject()
		testStorageClassMetadataCopyObject()
		testPutObjectWithContentLanguage()
		testListObjects()
		testRemoveObjects()
		testRemoveObjectsIter()
		testListObjectVersions()
		testStatObjectWithVersioning()
		testGetObjectWithVersioning()
		testCopyObjectWithVersioning()
		testConcurrentCopyObjectWithVersioning()
		testComposeObjectWithVersioning()
		testRemoveObjectWithVersioning()
		testRemoveObjectsWithVersioning()
		testObjectTaggingWithVersioning()
		testTrailingChecksums()
		testPutObjectWithAutomaticChecksums()
		testGetBucketTagging()
		testSetBucketTagging()
		testRemoveBucketTagging()

		// SSE-C tests will only work over TLS connection.
		if tls {
			testGetObjectAttributesSSECEncryption()
			testSSECEncryptionPutGet()
			testSSECEncryptionFPut()
			testSSECEncryptedGetObjectReadAtFunctional()
			testSSECEncryptedGetObjectReadSeekFunctional()
			testEncryptedCopyObjectV2()
			testEncryptedSSECToSSECCopyObject()
			testEncryptedSSECToUnencryptedCopyObject()
			testUnencryptedToSSECCopyObject()
			testUnencryptedToUnencryptedCopyObject()
			testEncryptedEmptyObject()
			testDecryptedCopyObject()
			testSSECEncryptedToSSECCopyObjectPart()
			testSSECMultipartEncryptedToSSECCopyObjectPart()
			testSSECEncryptedToUnencryptedCopyPart()
			testUnencryptedToSSECCopyObjectPart()
			testUnencryptedToUnencryptedCopyPart()
			testEncryptedSSECToSSES3CopyObject()
			testEncryptedSSES3ToSSECCopyObject()
			testSSECEncryptedToSSES3CopyObjectPart()
			testSSES3EncryptedToSSECCopyObjectPart()
		}

		// KMS tests
		if kms {
			testSSES3EncryptionPutGet()
			testSSES3EncryptionFPut()
			testSSES3EncryptedGetObjectReadAtFunctional()
			testSSES3EncryptedGetObjectReadSeekFunctional()
			testEncryptedSSES3ToSSES3CopyObject()
			testEncryptedSSES3ToUnencryptedCopyObject()
			testUnencryptedToSSES3CopyObject()
			testUnencryptedToSSES3CopyObjectPart()
			testSSES3EncryptedToUnencryptedCopyPart()
			testSSES3EncryptedToSSES3CopyObjectPart()
		}
	} else {
		testFunctional()
		testFunctionalV2()
	}
}
