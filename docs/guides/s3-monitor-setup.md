# S3 monitor — setup guide

The **S3 / Object storage** monitor asks one question: can this Phoenix pod, with
this key, reach **this bucket** (and optionally **this object**)?

It does **not** measure storage usage or quota. The S3 API has no cheap
`GetBucketSize`. Listing every object to sum sizes is expensive and is not
offered.

## What the check does

| `health_check`          | Request                                             | UP when  |
| ----------------------- | --------------------------------------------------- | -------- |
| `head_bucket` (default) | Signed `HEAD` of the bucket                         | HTTP 200 |
| `head_object`           | Signed `HEAD` of `object_key`                       | HTTP 200 |
| `get_object`            | Signed `GET` of `object_key` (reads at most 64 KiB) | HTTP 200 |

403, 404, timeouts, TLS failures, and region redirects are **DOWN**.

## Bucket names with `-` and `_`

- Hyphens (`my-backup`) work with path-style **or** virtual-hosted URLs.
- Underscores (`my_backup`) are **not valid in DNS hostnames**, so Phoenix
  always uses **path-style** (`https://endpoint/my_backup/...`). Unchecking
  path-style for those names is rejected at save time.
- AWS S3 itself does not allow `_` on new buckets. MinIO, Garage, and most
  S3-compatible stores do — that is why Phoenix accepts `_`.

Leave **Path-style addressing** on for MinIO and any custom endpoint.

## Connection

| Field               | Notes                                                                                                |
| ------------------- | ---------------------------------------------------------------------------------------------------- |
| Provider            | Hint only: `aws`, `minio`, or `generic` (R2, Wasabi, B2, Garage, Ceph RGW, DigitalOcean Spaces)      |
| Endpoint            | Empty = AWS `s3.<region>.amazonaws.com`. For MinIO include the scheme: `http://minio.minio.svc:9000` |
| Region              | SigV4 region. MinIO commonly `us-east-1`                                                             |
| Access / secret key | Required. Optional session token for STS                                                             |

Use **Skip TLS verify** (advanced) only for lab MinIO with a self-signed cert.

## Least-privilege IAM (read-only)

`HeadBucket` authorizes as `s3:ListBucket`. `HeadObject` / `GetObject` authorize
as `s3:GetObject`. Do **not** grant `s3:*` or any write action.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BucketProbe",
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::example-bucket"
    },
    {
      "Sid": "CanaryRead",
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::example-bucket/phoenix-canary"
    }
  ]
}
```

On MinIO, create a dedicated `phoenix_monitor` user with the same two actions
on one bucket.

## In-cluster MinIO example

- Endpoint: `http://minio.minio.svc:9000`
- Region: `us-east-1`
- Path-style: on
- Bucket: `velero` or `my_backup_bucket`
- Health check: `head_bucket`, or `head_object` + a canary key the backup job
  keeps in place

MinIO **cluster** health (`/minio/health/live`, `/minio/health/cluster`) is a
separate **HTTP** monitor. Do not mix process/quorum with bucket reachability.

## What this monitor will not do

- Report used bytes or object count
- List a prefix to find the newest backup
- Write a probe object (`PutObject`)
- Speak Azure Blob or native GCS
