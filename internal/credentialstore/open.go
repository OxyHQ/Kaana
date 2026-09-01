package credentialstore

import "context"

// Open connects Kaana's production PostgreSQL repository and KMS cipher.
func Open(ctx context.Context, databaseURL, kmsKeyARN string) (*Store, *Postgres, error) {
	repository, err := OpenPostgres(ctx, databaseURL)
	if err != nil {
		return nil, nil, err
	}
	cipher, err := OpenKMSCipher(ctx, kmsKeyARN)
	if err != nil {
		repository.Close()
		return nil, nil, err
	}
	store, err := New(repository, cipher)
	if err != nil {
		repository.Close()
		return nil, nil, err
	}
	return store, repository, nil
}
