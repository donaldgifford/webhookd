// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

// ClassifyK8sErrForTest re-exports classifyK8sErr for the
// `webhook_test` external package, which can't reach unexported
// symbols. Test-only — never call from production code.
func ClassifyK8sErrForTest(err error, namespace, name string) ExecResult {
	return classifyK8sErr(err, namespace, name)
}
