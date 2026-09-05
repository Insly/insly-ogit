package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/rs/zerolog"
)

// S3Storer implements go-git's storer.Storer interface using S3 as backend
type S3Storer struct {
	client   *awss3.Client
	bucket   string
	repoPath string
	logger   zerolog.Logger
	ctx      context.Context
}

// NewS3Storer creates a new S3-based storer for a specific repository
func NewS3Storer(client *awss3.Client, bucket, repoPath string, logger zerolog.Logger) *S3Storer {
	return &S3Storer{
		client:   client,
		bucket:   bucket,
		repoPath: repoPath,
		logger:   logger,
	}
}

// getObjectKey constructs the S3 key for a given path within the repository
func (s *S3Storer) getObjectKey(objectPath string) string {
	return path.Join(s.repoPath, objectPath)
}

// EncodedObject methods

// NewEncodedObject returns a new EncodedObject, the type must be specified
func (s *S3Storer) NewEncodedObject() plumbing.EncodedObject {
	return &plumbing.MemoryObject{}
}

// SetEncodedObject saves an EncodedObject to S3
func (s *S3Storer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	if obj.Type() == plumbing.OFSDeltaObject || obj.Type() == plumbing.REFDeltaObject {
		return plumbing.ZeroHash, plumbing.ErrInvalidType
	}

	// Read the object content
	reader, err := obj.Reader()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	hasher := plumbing.NewHasher(obj.Type(), int64(len(content)))
	_, _ = hasher.Write(content)
	hash := hasher.Sum()
	if obj.Size() != int64(len(content)) || obj.Hash() != hash {
		return plumbing.ZeroHash, fmt.Errorf("encoded object hash or size mismatch")
	}

	// Store in S3
	objectKey := s.getObjectKey(fmt.Sprintf("objects/%s/%s", hash.String()[:2], hash.String()[2:]))

	_, err = s.client.PutObject(s.context(), &awss3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(objectKey),
		IfNoneMatch: aws.String("*"),
		Body:        bytes.NewReader(content),
		Metadata: map[string]string{
			"git-type": obj.Type().String(),
		},
	})

	if statusCode(err) == 412 {
		_, err = s.EncodedObject(obj.Type(), hash)
	}
	return hash, err
}

// EncodedObject returns the EncodedObject with the given hash
func (s *S3Storer) EncodedObject(t plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	objectKey := s.getObjectKey(fmt.Sprintf("objects/%s/%s", hash.String()[:2], hash.String()[2:]))

	result, err := s.client.GetObject(s.context(), &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		if statusCode(err) == 404 {
			return nil, plumbing.ErrObjectNotFound
		}
		return nil, err
	}
	defer result.Body.Close()

	content, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, err
	}

	obj := &plumbing.MemoryObject{}

	objectType, err := plumbing.ParseObjectType(result.Metadata["git-type"])
	if err != nil {
		return nil, fmt.Errorf("missing or invalid git-type for %s", hash)
	}
	if t != plumbing.AnyObject && t != objectType {
		return nil, plumbing.ErrInvalidType
	}

	obj.SetType(objectType)
	obj.SetSize(int64(len(content)))
	obj.Write(content)

	if obj.Hash() != hash {
		return nil, fmt.Errorf("object hash mismatch: %s", hash)
	}
	return obj, nil
}

// IterEncodedObjects returns an iterator for all the objects in the repository
func (s *S3Storer) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	objectsPrefix := s.getObjectKey("objects/")

	var objects []plumbing.EncodedObject

	paginator := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(objectsPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(s.context())
		if err != nil {
			return nil, err
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if len(key) < len(objectsPrefix)+3 {
				continue
			}

			// Extract hash from key (objects/ab/cdef...)
			pathParts := strings.Split(key[len(objectsPrefix):], "/")
			if len(pathParts) != 2 {
				continue
			}
			hashStr := pathParts[0] + pathParts[1]

			hash := plumbing.NewHash(hashStr)
			encodedObj, err := s.EncodedObject(t, hash)
			if err == nil && (t == plumbing.AnyObject || encodedObj.Type() == t) {
				objects = append(objects, encodedObj)
			}
		}
	}

	return storer.NewEncodedObjectSliceIter(objects), nil
}

// HasEncodedObject returns true if the given hash is stored
func (s *S3Storer) HasEncodedObject(hash plumbing.Hash) error {
	objectKey := s.getObjectKey(fmt.Sprintf("objects/%s/%s", hash.String()[:2], hash.String()[2:]))

	_, err := s.client.HeadObject(s.context(), &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return plumbing.ErrObjectNotFound
	}

	return nil
}

// EncodedObjectSize returns the size of the encoded object
func (s *S3Storer) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	objectKey := s.getObjectKey(fmt.Sprintf("objects/%s/%s", hash.String()[:2], hash.String()[2:]))

	result, err := s.client.HeadObject(s.context(), &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return 0, plumbing.ErrObjectNotFound
	}

	return aws.ToInt64(result.ContentLength), nil
}

// DeleteEncodedObject removes the encoded object from S3
func (s *S3Storer) DeleteEncodedObject(hash plumbing.Hash) error {
	objectKey := s.getObjectKey(fmt.Sprintf("objects/%s/%s", hash.String()[:2], hash.String()[2:]))

	_, err := s.client.DeleteObject(s.context(), &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey),
	})

	return err
}

// Reference methods

// SetReference stores a reference
func (s *S3Storer) IterReferences() (storer.ReferenceIter, error) {
	refsPrefix := s.getObjectKey("refs/")

	var refs []*plumbing.Reference

	// First, add HEAD reference if it exists
	headRef, err := s.Reference(plumbing.HEAD)
	if err == nil {
		refs = append(refs, headRef)
	}

	// Then list all refs/ references
	paginator := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(refsPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(s.context())
		if err != nil {
			return nil, err
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			relativePath := key[len(s.getObjectKey("")):]
			// Remove leading slash if present
			if strings.HasPrefix(relativePath, "/") {
				relativePath = relativePath[1:]
			}
			refName := plumbing.ReferenceName(relativePath)

			s.logger.Debug().
				Str("key", key).
				Str("relativePath", relativePath).
				Str("refName", string(refName)).
				Msg("Processing reference in IterReferences")

			ref, err := s.Reference(refName)
			if err == nil {
				refs = append(refs, ref)
			} else {
				s.logger.Debug().
					Err(err).
					Str("refName", string(refName)).
					Msg("Failed to get reference in IterReferences")
			}
		}
	}

	return storer.NewReferenceSliceIter(refs), nil
}

// RemoveReference removes a reference
func (s *S3Storer) CountLooseRefs() (int, error) {
	iter, err := s.IterReferences()
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	count := 0
	err = iter.ForEach(func(*plumbing.Reference) error {
		count++
		return nil
	})

	return count, err
}

// Config methods

// Config returns the repository configuration
func (s *S3Storer) Config() (*config.Config, error) {
	objectKey := s.getObjectKey("config")

	result, err := s.client.GetObject(s.context(), &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		// Return default config if not found
		return &config.Config{}, nil
	}
	defer result.Body.Close()

	content, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, err
	}

	cfg := &config.Config{}
	err = cfg.Unmarshal(content)
	return cfg, err
}

// SetConfig sets the repository configuration
func (s *S3Storer) SetConfig(cfg *config.Config) error {
	objectKey := s.getObjectKey("config")

	content, err := cfg.Marshal()
	if err != nil {
		return err
	}

	_, err = s.client.PutObject(s.context(), &awss3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey),
		Body:   bytes.NewReader(content),
	})

	return err
}

// Index methods

// Index returns the repository index
func (s *S3Storer) Index() (*index.Index, error) {
	// Git bare repositories typically don't have an index
	return &index.Index{}, nil
}

// SetIndex sets the repository index
func (s *S3Storer) SetIndex(idx *index.Index) error {
	// Git bare repositories typically don't have an index
	return nil
}

// Shallow methods

// Shallow returns the shallow commits
func (s *S3Storer) Shallow() ([]plumbing.Hash, error) {
	objectKey := s.getObjectKey("shallow")

	result, err := s.client.GetObject(s.context(), &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, nil // No shallow file means no shallow commits
	}
	defer result.Body.Close()

	content, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	var hashes []plumbing.Hash

	for _, line := range lines {
		if line != "" {
			hashes = append(hashes, plumbing.NewHash(line))
		}
	}

	return hashes, nil
}

// SetShallow sets the shallow commits
func (s *S3Storer) SetShallow(hashes []plumbing.Hash) error {
	objectKey := s.getObjectKey("shallow")

	if len(hashes) == 0 {
		// Remove shallow file if no hashes
		_, err := s.client.DeleteObject(s.context(), &awss3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(objectKey),
		})
		return err
	}

	var content strings.Builder
	for _, hash := range hashes {
		content.WriteString(hash.String())
		content.WriteString("\n")
	}

	_, err := s.client.PutObject(s.context(), &awss3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectKey),
		Body:   strings.NewReader(content.String()),
	})

	return err
}

// Module returns the submodule storage, which we don't support for S3
func (s *S3Storer) Module(name string) (storer.Storer, error) {
	return nil, fmt.Errorf("submodules not supported in S3 storage")
}

// AddAlternate adds an alternate object database, which we don't support for S3
func (s *S3Storer) AddAlternate(remote string) error {
	return fmt.Errorf("alternates not supported in S3 storage")
}

// CheckAndSetReference atomically checks and sets a reference
func (s *S3Storer) PackRefs() error {
	// S3 storage doesn't need packed refs as each ref is a separate object
	return nil
}
