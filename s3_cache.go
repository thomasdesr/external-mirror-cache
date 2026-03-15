package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/thomasdesr/external-mirror-cache/internal/errorutil"
	"github.com/thomasdesr/external-mirror-cache/internal/reqlog"
)

type s3HTTPCache struct {
	s3c  *s3.Client
	s3pc *s3.PresignClient
	s3u  *transfermanager.Client

	bucket string
	prefix string
}

// Head checks to see if the provided key has been cached in S3 and if so
// returns its original request's HTTP headers.
func (c *s3HTTPCache) Head(ctx context.Context, key CacheKey) (http.Header, error) {
	s3Path := c.s3PathFor(key)
	logger := reqlog.FromContext(ctx)

	resp, err := c.s3c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(s3Path),
	})
	if err != nil {
		// Handle 404 NotFound gracefully because they're expected a lot of the
		// time.
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "NotFound" {
			logger.Debug("cache miss", "bucket", c.bucket, "key", s3Path)

			return nil, nil //nolint:nilnil // nil,nil is the cache interface's "not found" contract
		}

		return nil, errorutil.Wrapf(err, "HeadObject(%s, %s)", c.bucket, s3Path)
	}

	logger.Debug("cache hit", "bucket", c.bucket, "key", s3Path)

	return metadataToHeader(resp.Metadata)
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

	_, err = c.s3u.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(objectPath),
		Body:     body,
		Metadata: metadata,
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
