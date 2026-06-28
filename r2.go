package main

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Client struct {
	client     *s3.Client
	bucketName string
}

func InitR2(ctx context.Context, accountID, accessKeyID, secretAccessKey, bucketName string) (*R2Client, error) {
	// Endpoint for Cloudflare R2
	r2Endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
		config.WithRegion("auto"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load R2 configuration: %w", err)
	}

	// Create S3 client using custom endpoint resolution
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(r2Endpoint)
		o.UsePathStyle = true // Cloudflare R2 works best with path-style access
	})

	return &R2Client{
		client:     client,
		bucketName: bucketName,
	}, nil
}

func (r2 *R2Client) UploadFile(ctx context.Context, key string, body io.Reader, contentType string) error {
	_, err := r2.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(r2.bucketName),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("failed to upload object to R2: %w", err)
	}
	return nil
}

func (r2 *R2Client) DownloadFile(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := r2.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r2.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download object from R2: %w", err)
	}
	return out.Body, nil
}

func (r2 *R2Client) DeleteFile(ctx context.Context, key string) error {
	_, err := r2.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r2.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object from R2: %w", err)
	}
	return nil
}
