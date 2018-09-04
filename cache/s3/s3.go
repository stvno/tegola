package s3

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/go-spatial/tegola"
	"github.com/go-spatial/tegola/cache"
	"github.com/go-spatial/tegola/dict"
)

var (
	ErrMissingBucket = errors.New("s3cache: missing required param 'bucket'")
)

const CacheType = "s3"

const (
	// required
	ConfigKeyBucket = "bucket"
	// optional
	ConfigKeyBasepath       = "basepath"
	ConfigKeyMaxZoom        = "max_zoom"
	ConfigKeyRegion         = "region"   // defaults to "us-east-1"
	ConfigKeyEndpoint       = "endpoint" //	defaults to ""
	ConfigKeyAWSAccessKeyID = "aws_access_key_id"
	ConfigKeyAWSSecretKey   = "aws_secret_access_key"
	ConfigKeyACL            = "access_control_list" //	defaults to ""
	ConfigKeyCacheControl   = "cache_control"       //	defaults to ""
)

const (
	DefaultRegion = "us-east-1"
)

const (
	DefaultEndpoint = ""
)

func init() {
	cache.Register(CacheType, New)
}

// New instantiates a S3 cache. The config expects the following params:
//
// 	required:
// 		bucket (string): the name of the s3 bucket to write to
//
// 	optional:
// 		region (string): the AWS region the bucket is located. defaults to 'us-east-1'
// 		aws_access_key_id (string): an AWS access key id
// 		aws_secret_access_key (string): an AWS secret access key
// 		basepath (string): a path prefix added to all cache operations inside of the S3 bucket
// 		max_zoom (int): max zoom to use the cache. beyond this zoom cache Set() calls will be ignored
// 		endpoint (string): the endpoint where the S3 compliant backend is located. only necessary for non-AWS deployments. defaults to ''
//  	access_control_list (string): the S3 access control to set on the file when putting the file. defaults to ''.
//  	cache_control (string): the http cache-control header to set on the file when putting the file. defaults to ''.

func New(config dict.Dicter) (cache.Interface, error) {
	var err error

	s3cache := Cache{}

	// the config map's underlying value is int
	defaultMaxZoom := uint(tegola.MaxZ)
	maxZoom, err := config.Uint(ConfigKeyMaxZoom, &defaultMaxZoom)
	if err != nil {
		return nil, err
	}

	s3cache.MaxZoom = maxZoom

	s3cache.Bucket, err = config.String(ConfigKeyBucket, nil)
	if err != nil {
		return nil, ErrMissingBucket
	}
	if s3cache.Bucket == "" {
		return nil, ErrMissingBucket
	}

	// basepath
	basepath := ""
	s3cache.Basepath, err = config.String(ConfigKeyBasepath, &basepath)
	if err != nil {
		return nil, err
	}

	// check for region env var
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = DefaultRegion
	}
	region, err = config.String(ConfigKeyRegion, &region)
	if err != nil {
		return nil, err
	}

	accessKey := ""
	accessKey, err = config.String(ConfigKeyAWSAccessKeyID, &accessKey)
	if err != nil {
		return nil, err
	}
	secretKey := ""
	secretKey, err = config.String(ConfigKeyAWSSecretKey, &secretKey)
	if err != nil {
		return nil, err
	}

	awsConfig := aws.Config{
		Region: aws.String(region),
	}

	// check for endpoint env var
	endpoint := os.Getenv("AWS_ENDPOINT")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	endpoint, err = config.String(ConfigKeyEndpoint, &endpoint)
	if err != nil {
		return nil, err
	}

	// support for static credentials, this is not recommended by AWS but
	// necessary for some environments
	if accessKey != "" && secretKey != "" {
		awsConfig.Credentials = credentials.NewStaticCredentials(accessKey, secretKey, "")
	}

	// if an endpoint is set, add it to the awsConfig
	// otherwise do not set it and it will automatically use the correct aws-s3 endpoint
	if endpoint != "" {
		awsConfig.Endpoint = aws.String(endpoint)
	}

	// setup the s3 session.
	// if the accessKey and secreteKey are not provided (static creds) then the provider chain is used
	// http://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/configuring-sdk.html
	s3cache.Client = s3.New(
		session.New(&awsConfig),
	)

	// check for control_access_list env var
	acl := os.Getenv("AWS_ACL")
	acl, err = config.String(ConfigKeyACL, &acl)
	if err != nil {
		return nil, err
	}
	s3cache.ACL = acl

	// check for control_access_list env var
	cachecontrol := os.Getenv("AWS_CacheControl")
	cachecontrol, err = config.String(ConfigKeyCacheControl, &cachecontrol)
	if err != nil {
		return nil, err
	}
	s3cache.CacheControl = cachecontrol

	// in order to confirm we have the correct permissions on the bucket create a small file
	// and test a PUT, GET and DELETE to the bucket
	key := cache.Key{
		MapName:   "tegola-test-map",
		LayerName: "test-layer",
		Z:         0,
		X:         0,
		Y:         0,
	}
	// write a test file
	if err := s3cache.Set(&key, []byte("\x53\x69\x6c\x61\x73")); err != nil {
		e := cache.ErrSettingToCache{
			CacheType: CacheType,
			Err:       err,
		}

		return nil, e
	}

	// read the test file
	_, hit, err := s3cache.Get(&key)
	if err != nil {
		e := cache.ErrGettingFromCache{
			CacheType: CacheType,
			Err:       err,
		}

		return nil, e
	}
	if !hit {
		// return an error?
	}

	// purge the test file
	if err := s3cache.Purge(&key); err != nil {
		e := cache.ErrPurgingCache{
			CacheType: CacheType,
			Err:       err,
		}

		return nil, e
	}

	return &s3cache, nil
}

type Cache struct {
	// Bucket is the name of the s3 bucket to operate on
	Bucket string

	// Basepath is a path prefix added to all cache operations inside of the S3 bucket
	// helpful so a bucket does not need to be dedicated to only this cache
	Basepath string

	// MaxZoom determines the max zoom the cache to persist. Beyond this
	// zoom, cache Set() calls will be ignored. This is useful if the cache
	// should not be leveraged for higher zooms when data changes often.
	MaxZoom uint

	// client holds a reference to the s3 client. it's expected the client
	// has an active session and read, write, delete permissions have been checked
	Client *s3.S3

	// ACL is the aws ACL, if the not set it will use the default value for aws.
	ACL string

	// CacheControl is the http Cache Control header, if the not set it will use the default value for aws.
	CacheControl string

	// CacheControl is the http Cache Control header, if the not set it will use the default value for aws.
	ContentType string
}

func (s3c *Cache) Set(key *cache.Key, val []byte) error {
	var err error

	// check for maxzoom
	if key.Z > s3c.MaxZoom {
		return nil
	}

	// add our basepath
	k := filepath.Join(s3c.Basepath, key.String())

	input := s3.PutObjectInput{
		Body:        aws.ReadSeekCloser(bytes.NewReader(val)),
		Bucket:      aws.String(s3c.Bucket),
		Key:         aws.String(k),
		ContentType: aws.String("application/vnd.mapbox-vector-tile"),
	}
	if s3c.ACL != "" {
		input.ACL = aws.String(s3c.ACL)
	}
	if s3c.CacheControl != "" {
		input.CacheControl = aws.String(s3c.CacheControl)
	}

	_, err = s3c.Client.PutObject(&input)

	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			fmt.Println(awsErr.Code(), awsErr.Message(), awsErr.OrigErr())
			if reqErr, ok := err.(awserr.RequestFailure); ok {
				if reqErr.StatusCode() == 503 {
					time.Sleep(3 * time.Second)
					fmt.Println("sleeping.....")
					return nil
				}
			}
		}
		return err
	}

	return nil
}

func (s3c *Cache) Get(key *cache.Key) ([]byte, bool, error) {
	var err error

	// add our basepath
	k := filepath.Join(s3c.Basepath, key.String())

	input := s3.GetObjectInput{
		Bucket: aws.String(s3c.Bucket),
		Key:    aws.String(k),
	}

	result, err := s3c.Client.GetObject(&input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchKey:
				return nil, false, nil
			default:
				return nil, false, aerr
			}
		}
		return nil, false, err
	}

	var buf bytes.Buffer
	_, err = io.Copy(&buf, result.Body)
	if err != nil {
		return nil, false, err
	}

	return buf.Bytes(), true, nil
}

func (s3c *Cache) Purge(key *cache.Key) error {
	var err error

	// add our basepath
	k := filepath.Join(s3c.Basepath, key.String())

	input := s3.DeleteObjectInput{
		Bucket: aws.String(s3c.Bucket),
		Key:    aws.String(k),
	}

	_, err = s3c.Client.DeleteObject(&input)
	if err != nil {
		return err
	}

	return nil
}
