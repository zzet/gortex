package graph

// CheckInspection is pure: each caller supplies its own row count. The scope
// captures no mutable counter and can be shared across readers safely. Zero
// budget preserves existing direct-reader behavior when no identity exclusion
// was installed. Refusal concerns rows matching this query, not cohort size.
func (s LocalizationNodeScope) CheckInspection(inspected int) error {
	if s.identityInspectionLimit > 0 && inspected > s.identityInspectionLimit {
		return &BoundedLocalizationLimitError{Resource: "identity-filtered node inspections", Limit: s.identityInspectionLimit}
	}
	return nil
}

func (s LocalizationNodeScope) withIdentityExcluder(exclude func(string) bool) LocalizationNodeScope {
	if exclude == nil {
		return s
	}
	if s.excludeIdentity == nil {
		s.excludeIdentity = exclude
	} else {
		prior := s.excludeIdentity
		s.excludeIdentity = func(id string) bool { return prior(id) || exclude(id) }
	}
	if s.identityInspectionLimit == 0 || s.identityInspectionLimit > overlayExactNameInspectionLimit {
		s.identityInspectionLimit = overlayExactNameInspectionLimit
	}
	return s
}
