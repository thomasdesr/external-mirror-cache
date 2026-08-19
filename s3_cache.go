package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/thomasdesr/external-mirror-cache/internal/errorutil"
	"github.com/thomasdesr/external-mirror-cache/internal/reqlog"
)

// s3ObjectClient is the subset of s3.Client that s3HTTPCache calls directly.
type s3ObjectClient interface {
	HeadObject(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	CopyObject(ctx context.Context, input *s3.CopyObjectInput, opts ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
}

// s3Uploader is the subset of transfermanager.Client that s3HTTPCache needs.
type s3Uploader interface {
	UploadObject(
		ctx context.Context,
		input *transfermanager.UploadObjectInput,
		opts ...func(*transfermanager.Options),
	) (*transfermanager.UploadObjectOutput, error)
}

type s3HTTPCache struct {
	s3c  s3ObjectClient
	s3pc *s3.PresignClient
	s3u  s3Uploader

	bucket string
	prefix string
}

// Head checks to see if the provided key has been cached in S3 and if so
// returns its entry: the original response's HTTP headers plus the S3-side
// facts (LastModified, object ETag, size) the freshness gate and Touch read.
func (c *s3HTTPCache) Head(ctx context.Context, key CacheKey) (*cachedEntry, error) {
	s3Path := c.s3PathFor(key)
	logger := reqlog.FromContext(ctx)

	resp, err := c.s3c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(s3Path),
	})
	if err != nil {
		// NotFound (404) is the normal cache miss. Forbidden (403) happens when
		// the IAM role lacks s3:ListBucket — S3 returns 403 instead of 404 for
		// non-existent keys. Both mean "not in cache."
		if ae, ok := errors.AsType[smithy.APIError](err); ok && (ae.ErrorCode() == "NotFound" || ae.ErrorCode() == "Forbidden") {
			logger.Debug("cache miss", "bucket", c.bucket, "key", s3Path)

			return nil, nil //nolint:nilnil // nil,nil is the cache interface's "not found" contract
		}

		return nil, errorutil.Wrapf(err, "HeadObject(%s, %s)", c.bucket, s3Path)
	}

	logger.Debug("cache hit", "bucket", c.bucket, "key", s3Path)

	headers, err := metadataToHeader(resp.Metadata)
	if err != nil {
		return nil, err
	}

	return &cachedEntry{
		Headers:    headers,
		StoredAt:   aws.ToTime(resp.LastModified),
		ObjectETag: aws.ToString(resp.ETag),
		Size:       aws.ToInt64(resp.ContentLength),
	}, nil
}

// copyObjectSizeLimit is S3's single-part CopyObject cap. Objects above it
// skip the touch: no host in this traffic serves >5GB objects that declare
// freshness (the only plausible >5GB objects are OCI blobs, which never
// qualify), so no multipart-copy path is built. The skip log line naming a
// freshness-declaring host is the trigger for that follow-up design.
const copyObjectSizeLimit = 5 * 1024 * 1024 * 1024

// Touch re-arms a cached entry's freshness window: a CopyObject onto itself
// that advances LastModified to now and replaces the stored header metadata
// with the supplied set, without rewriting the body. The copy is conditional
// on the object still matching entry.ObjectETag, so a touch racing a
// concurrent Put from another instance fails with a 412 instead of rolling
// back the newer body or mislabeling it with old validators.
func (c *s3HTTPCache) Touch(ctx context.Context, key CacheKey, entry *cachedEntry, headers http.Header) error {
	s3Path := c.s3PathFor(key)
	logger := reqlog.FromContext(ctx)

	if entry.Size > copyObjectSizeLimit {
		logger.Info("touch skipped: object exceeds CopyObject limit",
			"bucket", c.bucket, "key", s3Path, "size", entry.Size)

		return nil
	}

	metadata, err := headerToMetadata(headers)
	if err != nil {
		return errorutil.Wrapf(err, "headerToMetadata(%v)", headers)
	}

	// CopyObject's REPLACE directive resets system metadata not explicitly
	// carried, so ContentType must be re-supplied the same way Put sets it —
	// otherwise the first touch would degrade every object's Content-Type to
	// binary/octet-stream.
	var contentType *string
	if ct := headers.Get("Content-Type"); ct != "" {
		contentType = aws.String(ct)
	}

	_, err = c.s3c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(c.bucket),
		Key:               aws.String(s3Path),
		CopySource:        aws.String(escapeCopySource(c.bucket + "/" + s3Path)),
		CopySourceIfMatch: aws.String(entry.ObjectETag),
		MetadataDirective: types.MetadataDirectiveReplace,
		Metadata:          metadata,
		ContentType:       contentType,
	})
	if err != nil {
		return errorutil.Wrapf(err, "CopyObject(%s, %s)", c.bucket, s3Path)
	}

	logger.Debug("touched cache entry", "bucket", c.bucket, "key", s3Path)

	return nil
}

// escapeCopySource percent-encodes a bucket/key path for the
// x-amz-copy-source header, leaving only RFC 3986 unreserved characters and
// '/' literal. S3 applies query-style decoding to this header — a literal
// '+' reads back as a space, so a URL-path encoding (which keeps '+') makes
// the copy 404 for any key containing one (PyPI local-version wheels like
// torch-2.1.0+cpu). First-party SDKs (botocore, Java v2) encode this same
// character set.
func escapeCopySource(s string) string {
	var b strings.Builder

	for i := range len(s) {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}

	return b.String()
}

// GetPresignedURL returns a presigned S3 URL for the provided key. This does
// not check if said URL exists.
func (c *s3HTTPCache) GetPresignedURL(ctx context.Context, key CacheKey) (string, error) {
	objectPath := c.s3PathFor(key)
	logger := reqlog.FromContext(ctx)

	presignedResponse, err := c.s3pc.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectPath),
	})
	if err != nil {
		return "", errorutil.Wrapf(err, "PresignGetObject(%s, %s)", c.bucket, objectPath)
	}

	logger.Debug("presigned URL generated", "bucket", c.bucket, "key", objectPath)

	return presignedResponse.URL, nil
}

// Put uploads the provided body to the appropriate path in S3 based on the
// provided key and attaches its headers as S3 Object metadata.
func (c *s3HTTPCache) Put(ctx context.Context, key CacheKey, headers http.Header, body io.Reader) error {
	objectPath := c.s3PathFor(key)
	logger := reqlog.FromContext(ctx)

	metadata, err := headerToMetadata(headers)
	if err != nil {
		return errorutil.Wrapf(err, "headerToMetadata(%v)", headers)
	}

	logger.Debug("uploading to cache", "bucket", c.bucket, "key", objectPath)

	var contentType *string
	if ct := headers.Get("Content-Type"); ct != "" {
		contentType = aws.String(ct)
	}

	_, err = c.s3u.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(objectPath),
		Body:        body,
		Metadata:    metadata,
		ContentType: contentType,
	})
	if err != nil {
		return errorutil.Wrapf(err, "UploadObject(%s, %s)", c.bucket, objectPath)
	}

	logger.Debug("upload complete", "bucket", c.bucket, "key", objectPath)

	return nil
}

func (c *s3HTTPCache) s3PathFor(key CacheKey) string {
	u := key.URL

	path := strings.Join([]string{c.prefix, u.Host, strings.TrimPrefix(u.Path, "/")}, "/")
	if u.RawQuery != "" {
		path += "?" + url.QueryEscape(u.RawQuery)
	}

	if key.Variant != "" {
		path += "//" + url.PathEscape(key.Variant)
	}

	return path
}
