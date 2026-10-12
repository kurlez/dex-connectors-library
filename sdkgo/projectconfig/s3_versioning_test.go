// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"

	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

// TestS3ObjectStoreReportsWhyVersioningIsUnusable checks that opening a store names the cause: the provider's
// refusal of the versioning read, or the bucket's actual versioning status.
func TestS3ObjectStoreReportsWhyVersioningIsUnusable(t *testing.T) {
	cases := []struct {
		name              string
		status            int
		body              string
		wantRequired      bool
		wantMessageDetail string
	}{
		{
			name:   "refused read",
			status: http.StatusForbidden,
			body: `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code>` +
				`<Message>not authorized to perform s3:GetBucketVersioning</Message></Error>`,
			wantMessageDetail: "AccessDenied",
		},
		{
			name:              "suspended",
			status:            http.StatusOK,
			body:              `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`,
			wantRequired:      true,
			wantMessageDetail: "versioning is Suspended",
		},
		{
			name:              "never enabled",
			status:            http.StatusOK,
			body:              `<VersioningConfiguration></VersioningConfiguration>`,
			wantRequired:      true,
			wantMessageDetail: "versioning is never enabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Contains(t, r.URL.RawQuery, "versioning")
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := projectconfig.NewS3ObjectStore(context.Background(), &projectconfig.S3StoreConfig{
				Client: versioningTestClient(server.URL), Bucket: "project-bucket", AllowUnencrypted: true,
			})
			require.Error(t, err)
			require.Equal(t, tc.wantRequired, errors.Is(err, projectconfig.ErrBucketVersioningRequired))
			require.Contains(t, err.Error(), tc.wantMessageDetail)
			require.Contains(t, err.Error(), "project-bucket")
		})
	}
}

func TestS3ObjectStoreAcceptsEnabledVersioning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`))
	}))
	defer server.Close()

	_, err := projectconfig.NewS3ObjectStore(context.Background(), &projectconfig.S3StoreConfig{
		Client: versioningTestClient(server.URL), Bucket: "project-bucket", AllowUnencrypted: true,
	})
	require.NoError(t, err)
}

func versioningTestClient(endpoint string) *s3.Client {
	return s3.New(s3.Options{
		Region:       "us-west-2",
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  aws.AnonymousCredentials{},
		// A refused request is the outcome under test, not a transient failure to retry.
		RetryMaxAttempts: 1,
	})
}
