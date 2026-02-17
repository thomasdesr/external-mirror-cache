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
)

type s3HTTPCache struct {
	s3c  *s3.Client
	s3pc *s3.PresignClient
	s3u  *transfermanager.Client

	bucket string
	prefix string
}

// Head checks to see if the provided URL has been cached in S3 and if so
// returns its original request's HTTP headers.
func (c *s3HTTPCache) Head(ctx context.Context, url *url.URL) (http.Header, error) {
	s3Path := c.s3PathFor(url)

	resp, err := c.s3c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(s3Path),
	})
	if err != nil {
		// Handle 404 NotFound gracefully because they're expected a lot of the
		// time.
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "NotFound" {
			return nil, nil //nolint:nilnil // nil,nil is the cache interface's "not found" contract
		}

		return nil, errorutil.Wrapf(err, "HeadObject(%s, %s)", c.bucket, s3Path)
	}

	return metadataToHeader(resp.Metadata)
}

// GetPresignedURL returns a presigned S3 URL for the provided URL. This does
// not check if said URL exists.
func (c *s3HTTPCache) GetPresignedURL(ctx context.Context, url *url.URL) (string, error) {
	objectPath := c.s3PathFor(url)

	presignedResponse, err := c.s3pc.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectPath),
	})
	if err != nil {
		return "", errorutil.Wrapf(err, "PresignGetObject(%s, %s)", c.bucket, objectPath)
	}

	return presignedResponse.URL, nil
}

// Put uploads the provided body to the appropriate path in S3 based on the
// provided URL and attaches its headers as S3 Object metadata.
func (c *s3HTTPCache) Put(ctx context.Context, url *url.URL, headers http.Header, body io.Reader) error {
	objectPath := c.s3PathFor(url)

	metadata, err := headerToMetadata(headers)
	if err != nil {
		return errorutil.Wrapf(err, "headerToMetadata(%v)", headers)
	}

	_, err = c.s3u.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(objectPath),
		Body:     body,
		Metadata: metadata,
	})
	if err != nil {
		return errorutil.Wrapf(err, "UploadObject(%s, %s)", c.bucket, objectPath)
	}

	return nil
}

func (c *s3HTTPCache) s3PathFor(u *url.URL) string {
	path := strings.Join([]string{c.prefix, u.Host, strings.TrimPrefix(u.Path, "/")}, "/")
	if u.RawQuery != "" {
		path += "?" + url.QueryEscape(u.RawQuery)
	}

	return path
}
