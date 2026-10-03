package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/QuantumNous/astrlink/core/contract"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

func (store *Store) CreateIdentityProfile(ctx context.Context, candidate contract.IdentityProfile) (storagecontract.IdentityProfileRecord, error) {
	candidate = candidate.Clone()
	if candidate.ConfirmedAt != nil {
		return storagecontract.IdentityProfileRecord{}, fmt.Errorf("%w: new profiles must be unconfirmed", storagecontract.ErrInvalidArgument)
	}
	candidate.CreatedAt = store.now().UTC()
	if err := candidate.Validate(); err != nil {
		return storagecontract.IdentityProfileRecord{}, fmt.Errorf("%w: invalid identity candidate", storagecontract.ErrInvalidArgument)
	}
	if err := store.identityProfileService(ctx, candidate.ServiceID); err != nil {
		return storagecontract.IdentityProfileRecord{}, err
	}
	document, err := json.Marshal(candidate)
	if err != nil {
		return storagecontract.IdentityProfileRecord{}, err
	}
	// A single statement keeps creation atomic with deletion of the service.
	// Kind is immutable, but recheck the HTTP payload in case the ID was reused.
	result, err := store.db.ExecContext(ctx, `INSERT INTO service_identity_profiles (id, service_id, document_json)
SELECT ?, id, ? FROM services WHERE id = ? AND json_type(document_json, '$.http') = 'object'`,
		candidate.ID, string(document), candidate.ServiceID)
	if err != nil {
		var exists int
		if queryErr := store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM service_identity_profiles WHERE id = ?)`, candidate.ID).Scan(&exists); queryErr == nil && exists == 1 {
			return storagecontract.IdentityProfileRecord{}, storagecontract.ErrConflict
		}
		return storagecontract.IdentityProfileRecord{}, fmt.Errorf("create identity profile: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return storagecontract.IdentityProfileRecord{}, err
	}
	if count != 1 {
		return storagecontract.IdentityProfileRecord{}, storagecontract.ErrNotFound
	}
	return storagecontract.IdentityProfileRecord{Profile: candidate, ETag: entityTag(document)}, nil
}

func (store *Store) GetIdentityProfile(ctx context.Context, serviceID contract.ServiceID, id contract.IdentityProfileID) (storagecontract.IdentityProfileRecord, error) {
	if serviceID.Validate() != nil || id.Validate() != nil {
		return storagecontract.IdentityProfileRecord{}, storagecontract.ErrInvalidArgument
	}
	var document string
	if err := store.db.QueryRowContext(ctx, `SELECT document_json FROM service_identity_profiles WHERE service_id = ? AND id = ?`, serviceID, id).Scan(&document); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storagecontract.IdentityProfileRecord{}, storagecontract.ErrNotFound
		}
		return storagecontract.IdentityProfileRecord{}, fmt.Errorf("get identity profile: %w", err)
	}
	return decodeIdentityProfile(serviceID, id, []byte(document))
}

func (store *Store) ListIdentityProfiles(ctx context.Context, serviceID contract.ServiceID, options storagecontract.IdentityProfileListOptions) (storagecontract.IdentityProfilePage, error) {
	if err := store.identityProfileService(ctx, serviceID); err != nil {
		return storagecontract.IdentityProfilePage{}, err
	}
	limit := options.Limit
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maxListLimit {
		return storagecontract.IdentityProfilePage{}, storagecontract.ErrInvalidArgument
	}
	after, err := decodeCursor(options.Cursor)
	if err != nil {
		return storagecontract.IdentityProfilePage{}, err
	}
	if after != "" && contract.IdentityProfileID(after).Validate() != nil {
		return storagecontract.IdentityProfilePage{}, storagecontract.ErrInvalidCursor
	}
	rows, err := store.db.QueryContext(ctx, `SELECT id, document_json FROM service_identity_profiles WHERE service_id = ? AND id > ? ORDER BY id LIMIT ?`, serviceID, after, limit+1)
	if err != nil {
		return storagecontract.IdentityProfilePage{}, fmt.Errorf("list identity profiles: %w", err)
	}
	defer rows.Close()
	page := storagecontract.IdentityProfilePage{Items: make([]storagecontract.IdentityProfileRecord, 0)}
	for rows.Next() {
		var id contract.IdentityProfileID
		var document string
		if err := rows.Scan(&id, &document); err != nil {
			return storagecontract.IdentityProfilePage{}, err
		}
		record, err := decodeIdentityProfile(serviceID, id, []byte(document))
		if err != nil {
			return storagecontract.IdentityProfilePage{}, err
		}
		page.Items = append(page.Items, record)
	}
	if err := rows.Err(); err != nil {
		return storagecontract.IdentityProfilePage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = encodeCursor(contract.ServiceID(page.Items[limit-1].Profile.ID))
	}
	return page, nil
}

func (store *Store) ConfirmIdentityProfile(ctx context.Context, serviceID contract.ServiceID, id contract.IdentityProfileID, expectedETag string) (storagecontract.IdentityProfileRecord, error) {
	if expectedETag == "" {
		return storagecontract.IdentityProfileRecord{}, storagecontract.ErrInvalidArgument
	}
	record, err := store.GetIdentityProfile(ctx, serviceID, id)
	if err != nil {
		return storagecontract.IdentityProfileRecord{}, err
	}
	if record.ETag != expectedETag {
		return storagecontract.IdentityProfileRecord{}, storagecontract.ErrPrecondition
	}
	if record.Profile.ConfirmedAt != nil {
		return record, nil
	}
	now := store.now().UTC()
	record.Profile.ConfirmedAt = &now
	if err := record.Profile.Validate(); err != nil {
		return storagecontract.IdentityProfileRecord{}, fmt.Errorf("%w: invalid confirmation time", storagecontract.ErrInvalidArgument)
	}
	document, err := json.Marshal(record.Profile)
	if err != nil {
		return storagecontract.IdentityProfileRecord{}, err
	}
	// Match the reviewed ETag to the original bytes, then compare those bytes
	// atomically in the UPDATE. No read transaction is held while another
	// confirmer writes, avoiding deferred-transaction upgrade races.
	result, err := store.compareAndSetIdentityProfile(ctx, serviceID, id, expectedETag, string(document), false)
	if err != nil {
		return storagecontract.IdentityProfileRecord{}, err
	}
	if !result {
		return storagecontract.IdentityProfileRecord{}, storagecontract.ErrPrecondition
	}
	record.ETag = entityTag(document)
	return record, nil
}

func (store *Store) DiscardIdentityProfile(ctx context.Context, serviceID contract.ServiceID, id contract.IdentityProfileID, expectedETag string) error {
	if expectedETag == "" {
		return storagecontract.ErrInvalidArgument
	}
	record, err := store.GetIdentityProfile(ctx, serviceID, id)
	if err != nil {
		return err
	}
	if record.ETag != expectedETag {
		return storagecontract.ErrPrecondition
	}
	if record.Profile.ConfirmedAt != nil {
		return storagecontract.ErrConflict
	}
	changed, err := store.compareAndSetIdentityProfile(ctx, serviceID, id, expectedETag, "", true)
	if err != nil {
		return err
	}
	if !changed {
		return storagecontract.ErrPrecondition
	}
	return nil
}

func (store *Store) compareAndSetIdentityProfile(ctx context.Context, serviceID contract.ServiceID, id contract.IdentityProfileID, etag, replacement string, discard bool) (bool, error) {
	var original string
	if err := store.db.QueryRowContext(ctx, `SELECT document_json FROM service_identity_profiles WHERE service_id = ? AND id = ?`, serviceID, id).Scan(&original); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if entityTag([]byte(original)) != etag {
		return false, nil
	}
	var result sql.Result
	var err error
	if discard {
		result, err = store.db.ExecContext(ctx, `DELETE FROM service_identity_profiles WHERE service_id = ? AND id = ? AND document_json = ?`, serviceID, id, original)
	} else {
		result, err = store.db.ExecContext(ctx, `UPDATE service_identity_profiles SET document_json = ? WHERE service_id = ? AND id = ? AND document_json = ?`, replacement, serviceID, id, original)
	}
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (store *Store) identityProfileService(ctx context.Context, id contract.ServiceID) error {
	record, err := store.GetService(ctx, id)
	if err != nil {
		return err
	}
	if !record.Service.Kind.IsHTTP() || record.Service.HTTP == nil {
		return fmt.Errorf("%w: identity profiles require an HTTP service", storagecontract.ErrInvalidArgument)
	}
	return nil
}

func decodeIdentityProfile(serviceID contract.ServiceID, id contract.IdentityProfileID, document []byte) (storagecontract.IdentityProfileRecord, error) {
	if len(document) > 16<<10 {
		return storagecontract.IdentityProfileRecord{}, storagecontract.ErrInvalidRecord
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var profile contract.IdentityProfile
	if err := decoder.Decode(&profile); err != nil {
		return storagecontract.IdentityProfileRecord{}, storagecontract.ErrInvalidRecord
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || profile.ID != id || profile.ServiceID != serviceID || profile.Validate() != nil {
		return storagecontract.IdentityProfileRecord{}, storagecontract.ErrInvalidRecord
	}
	return storagecontract.IdentityProfileRecord{Profile: profile, ETag: entityTag(document)}, nil
}

var _ storagecontract.IdentityProfileStore = (*Store)(nil)
