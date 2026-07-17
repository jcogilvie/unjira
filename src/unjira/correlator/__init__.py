"""Deterministic correlation primitives.

These are the cheap, LLM-free pre-filters that run *before* the phase-1 LLM
correlator so its review queue stays signal-rich (see docs/design-notes.md):
fully-qualified range-aware reference dedup (refs) and env-mirror fan-out
clustering (fanout). Pure functions, no I/O, no Jira dependency.
"""
