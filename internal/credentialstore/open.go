package credentialstore

import "context"

// Open connects Kaana's production PostgreSQL repository and KMS cipher.
func Open(ctx context.Context, databaseURL, kmsKeyARN string) (*Store, *Postgres, error) {
	store, repository, _, err := open(ctx, databaseURL, kmsKeyARN)
	return store, repository, err
}

// OpenRuntime connects the same ciphertext-only PostgreSQL/KMS boundary as
// Open and additionally returns the inference-only customer credential
// resolver. Whether it can Encrypt or Decrypt is still decided by the task's
// IAM role; this constructor grants no AWS authority of its own.
func OpenRuntime(ctx context.Context, databaseURL, kmsKeyARN string) (*Store, *CustomerResolver, *Postgres, error) {
	store, repository, cipher, err := open(ctx, databaseURL, kmsKeyARN)
	if err != nil {
		return nil, nil, nil, err
	}
	resolver, err := NewCustomerResolver(repository, cipher)
	if err != nil {
		repository.Close()
		return nil, nil, nil, err
	}
	return store, resolver, repository, nil
}

func open(ctx context.Context, databaseURL, kmsKeyARN string) (*Store, *Postgres, *KMSCipher, error) {
	repository, err := OpenPostgres(ctx, databaseURL)
	if err != nil {
		return nil, nil, nil, err
	}
	cipher, err := OpenKMSCipher(ctx, kmsKeyARN)
	if err != nil {
		repository.Close()
		return nil, nil, nil, err
	}
	store, err := New(repository, cipher)
	if err != nil {
		repository.Close()
		return nil, nil, nil, err
	}
	return store, repository, cipher, nil
}
