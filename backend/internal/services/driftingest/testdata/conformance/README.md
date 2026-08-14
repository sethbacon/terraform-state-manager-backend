# Vendored conformance corpus — do not edit here

`vectors.json` is a **byte-identical copy** of `conformance/vectors.json` in
[4cloudguru/terraform-drift-contract](https://github.com/4cloudguru/terraform-drift-contract),
which is the canonical side of the drift contract and the only place the
expectations are authored.

It is run by three implementations:

| Implementation | Runner |
| --- | --- |
| TypeScript (canonical) | the contract repo — `__tests__/conformance.test.ts` |
| Go `driftingest` | `../../conformance_test.go` |
| jq (dispatched CI templates) | `../../../../api/drift_conformance_test.go` |

`conformance_test.go` pins the file's SHA-256 and a digest over the rendered
results, using the **same literals** the contract's runner asserts. Editing this
copy alone reddens this repository; editing the contract's copy alone reddens
that one. That is the mechanism — neither CI job can run the other language, so
the shared literals are what make a divergence visible.

To change it, follow `conformance/README.md` in the contract repository: the
semantic change, the vector and the digests move together, in both repositories,
in the same batch.
