-- 1370_no_deferred_prune (2.4.0, §0a, IN-8) — Global's repository is not a
-- library. A script or a schedule whose file leaves Global's repository is
-- pruned at Global's sync, as any repository's is: no other repository's
-- definition can use it, so nothing waits for one (GR-17's deferred prune is
-- gone). The notices that said a prune was waiting had nothing left to clear
-- them; they go.

DELETE FROM notices WHERE kind = 'prune_deferred';
