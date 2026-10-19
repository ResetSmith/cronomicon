-- 1590_close_ssh_queued (down). Nothing: the closed runs stay closed. Which
-- rows the up step closed is not recorded apart from their reason, and putting
-- them back in the queue would only block Global's scheduling again (see the up
-- step). Lossy by design.
SELECT 1;
