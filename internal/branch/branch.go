package branch

import (
	"context"
	"fmt"

	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
	"github.com/litebase/litebase/internal/manager"
	"github.com/litebase/litebase/internal/migrate"
	"github.com/litebase/litebase/internal/snapshot"
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

func (s *Service) Create(
	ctx context.Context,
	parentDBName string,
	branchName string,
) (control.BranchRecord, control.DatabaseRecord, error) {
	if err := engine.ValidateName(parentDBName); err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	if err := engine.ValidateName(branchName); err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	parentEntry, err := s.mgr.Acquire(ctx, parentDBName)
	if err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	defer s.mgr.Release(parentEntry)

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
	if err := parentEntry.DB.VacuumInto(ctx, branchDBPath); err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, fmt.Errorf("copy parent database into branch %q: %w", branchName, err)
	}
	branchDBRec, err := s.mgr.CreateDatabaseAtPath(ctx, branchName, branchDBPath, parentEntry.Record.Durability)
	if err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	branchRec, err := s.mgr.Store().CreateBranchRecord(ctx, control.CreateBranchParams{
		BranchDatabaseID: branchDBRec.ID,
		ParentDatabaseID: parentEntry.Record.ID,
		ParentSnapshotID: snapRec.ID,
		Name:             branchName,
	})
	if err != nil {
		return control.BranchRecord{}, control.DatabaseRecord{}, err
	}
	return branchRec, branchDBRec, nil
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
	return s.mgr.DeleteDatabase(ctx, branchName)
}

func (s *Service) DiffSchema(ctx context.Context, parentDBName string, branchName string) ([]string, error) {
	parentSchema, err := s.inspectDatabaseSchema(ctx, parentDBName)
	if err != nil {
		return nil, err
	}
	branchSchema, err := s.inspectDatabaseSchema(ctx, branchName)
	if err != nil {
		return nil, err
	}
	return migrate.GenerateSchemaDiffSQL(parentSchema, branchSchema), nil
}

func (s *Service) MergeSchema(ctx context.Context, parentDBName string, branchName string) ([]string, error) {
	diffStatements, err := s.DiffSchema(ctx, parentDBName, branchName)
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

func (s *Service) inspectDatabaseSchema(ctx context.Context, dbName string) ([]migrate.SchemaObject, error) {
	entry, err := s.mgr.Acquire(ctx, dbName)
	if err != nil {
		return nil, err
	}
	defer s.mgr.Release(entry)
	return migrate.InspectSchema(ctx, entry.DB)
}
