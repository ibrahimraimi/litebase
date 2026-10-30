package branch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/migrate"
	"github.com/ibrahimraimi/litebase/internal/snapshot"
)

type Service struct {
	mgr     *manager.Manager
	snapSvc *snapshot.Service
}

func NewService(mgr *manager.Manager, snapSvc *snapshot.Service) *Service {
	return &Service{
		mgr:     mgr,
		snapSvc: snapSvc,
	}
}

func FormatBranchDatabaseName(parentDBName string, branchName string) string {
	return parentDBName + "--" + branchName
}

func validateParentAndBranchNames(parentDBName string, branchName string) error {
	if err := engine.ValidateName(parentDBName); err != nil {
		return err
	}
	if err := engine.ValidateName(branchName); err != nil {
		return err
	}
	if strings.Contains(parentDBName, "--") || strings.Contains(branchName, "--") {
		return fmt.Errorf("%w: names cannot contain '--'", engine.ErrInvalidIdentifier)
	}
	return engine.ValidateName(FormatBranchDatabaseName(parentDBName, branchName))
}

func (s *Service) Create(
	ctx context.Context,
	parentDBName string,
	branchName string,
) (control.BranchRecord, control.DatabaseRecord, error) {
	if err := validateParentAndBranchNames(parentDBName, branchName); err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	parentEntry, err := s.mgr.Acquire(ctx, parentDBName)
	if err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	parentRecord := parentEntry.Record
	s.mgr.Release(parentEntry)

	snapRec, err := s.snapSvc.Create(ctx, parentDBName, "branch-"+branchName)
	if err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	branchDBPath, err := engine.SafeJoinPath(
		s.mgr.DataDir(),
		"databases",
		control.DefaultProjectName,
		parentDBName,
		"branches",
		branchName+".sqlite",
	)
	if err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	if err := copySnapshotFile(snapRec.Path, branchDBPath); err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, fmt.Errorf("copy snapshot into branch %q: %w", branchName, err)
	}
	internalBranchDBName := FormatBranchDatabaseName(parentDBName, branchName)
	branchDBRec, err := s.mgr.CreateDatabaseAtPath(ctx, internalBranchDBName, branchDBPath, parentRecord.Durability)
	if err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	branchRec, err := s.mgr.Store().CreateBranchRecord(ctx, control.CreateBranchParams{
		BranchDatabaseID: branchDBRec.ID,
		ParentDatabaseID: parentRecord.ID,
		ParentSnapshotID: snapRec.ID,
		Name:             branchName,
	})
	if err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	return branchRec, branchDBRec, nil
}

func copySnapshotFile(srcPath string, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
		return err
	}
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		_ = dstFile.Close()
		_ = os.Remove(dstPath)
		return err
	}
	if err := dstFile.Sync(); err != nil {
		_ = dstFile.Close()
		_ = os.Remove(dstPath)
		return err
	}
	return dstFile.Close()
}

func (s *Service) List(ctx context.Context, parentDBName string) ([]control.BranchRecord, error) {
	if err := engine.ValidateName(parentDBName); err != nil {
		return nil, err
	}
	parentRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, parentDBName)
	if err != nil {
		return nil, err
	}
	return s.mgr.Store().ListBranchRecords(ctx, parentRec.ID)
}

func (s *Service) Delete(ctx context.Context, parentDBName string, branchName string) error {
	if err := engine.ValidateName(parentDBName); err != nil {
		return err
	}
	if err := engine.ValidateName(branchName); err != nil {
		return err
	}
	parentRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, parentDBName)
	if err != nil {
		return err
	}
	if err := s.mgr.Store().DeleteBranchRecord(ctx, parentRec.ID, branchName); err != nil {
		return err
	}
	err = s.mgr.DeleteDatabase(ctx, FormatBranchDatabaseName(parentDBName, branchName))
	if errors.Is(err, control.ErrNotFound) {
		return s.mgr.DeleteDatabase(ctx, branchName)
	}
	return err
}

func (s *Service) DiffSchema(ctx context.Context, parentDBName string, branchName string) ([]string, error) {
	return s.DiffSchemaWithOptions(ctx, parentDBName, branchName, migrate.DiffOptions{})
}

func (s *Service) DiffSchemaWithOptions(
	ctx context.Context,
	parentDBName string,
	branchName string,
	opts migrate.DiffOptions,
) ([]string, error) {
	baseSchema, err := s.loadBaseSnapshotSchema(ctx, parentDBName, branchName)
	if err != nil {
		return nil, err
	}
	oursSchema, err := s.inspectDatabaseSchema(ctx, parentDBName)
	if err != nil {
		return nil, err
	}
	theirsSchema, err := s.inspectBranchSchema(ctx, parentDBName, branchName)
	if err != nil {
		return nil, err
	}
	return migrate.ComputeThreeWaySchemaDiff(baseSchema, oursSchema, theirsSchema, opts)
}

func (s *Service) MergeSchema(ctx context.Context, parentDBName string, branchName string) ([]string, error) {
	return s.MergeSchemaWithOptions(ctx, parentDBName, branchName, migrate.DiffOptions{})
}

func (s *Service) MergeSchemaWithOptions(
	ctx context.Context,
	parentDBName string,
	branchName string,
	opts migrate.DiffOptions,
) ([]string, error) {
	diffStatements, err := s.DiffSchemaWithOptions(ctx, parentDBName, branchName, opts)
	if err != nil {
		return nil, err
	}
	if len(diffStatements) == 0 {
		return []string{}, nil
	}
	parentEntry, err := s.mgr.Acquire(ctx, parentDBName)
	if err != nil {
		return nil, err
	}
	defer s.mgr.Release(parentEntry)

	batch := make([]engine.Statement, 0, len(diffStatements))
	for _, sqlText := range diffStatements {
		for _, single := range engine.SplitSQLStatements(sqlText) {
			batch = append(batch, engine.Statement{SQL: single})
		}
	}
	if _, err := parentEntry.DB.ExecuteBatch(ctx, batch); err != nil {
		return nil, fmt.Errorf("apply branch schema merge to %q: %w", parentDBName, err)
	}
	return diffStatements, nil
}

func (s *Service) loadBaseSnapshotSchema(
	ctx context.Context,
	parentDBName string,
	branchName string,
) ([]migrate.SchemaObject, error) {
	parentRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, parentDBName)
	if err != nil {
		return nil, err
	}
	branchRec, err := s.mgr.Store().GetBranchRecord(ctx, parentRec.ID, branchName)
	if err != nil {
		return nil, err
	}
	if branchRec.ParentSnapshotID == "" {
		return s.inspectDatabaseSchema(ctx, parentDBName)
	}
	snapRec, err := s.mgr.Store().GetSnapshotRecord(ctx, parentRec.ID, branchRec.ParentSnapshotID)
	if err != nil {
		return nil, err
	}
	return migrate.InspectSchemaFile(ctx, snapRec.Path)
}

func (s *Service) inspectBranchSchema(
	ctx context.Context,
	parentDBName string,
	branchName string,
) ([]migrate.SchemaObject, error) {
	namespaced := FormatBranchDatabaseName(parentDBName, branchName)
	schema, err := s.inspectDatabaseSchema(ctx, namespaced)
	if errors.Is(err, control.ErrNotFound) {
		return s.inspectDatabaseSchema(ctx, branchName)
	}
	return schema, err
}

func (s *Service) inspectDatabaseSchema(ctx context.Context, dbName string) ([]migrate.SchemaObject, error) {
	entry, err := s.mgr.Acquire(ctx, dbName)
	if err != nil {
		return nil, err
	}
	defer s.mgr.Release(entry)
	return migrate.InspectSchema(ctx, entry.DB)
}
