// The feedback kit (github.com/jbenet/feedback-dev-kit) as one script for
// `ipfs2fil explore --feedback`. The explore server adds it to the explorer's
// pages only in that mode; archives never carry it. Built into
// ../assets/feedback.js by scripts/build-feedback.sh.
import type { ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import {
  FeedbackButton, FeedbackProvider, FeedbackStatus, IssuePage, IssuesPage, VIEWPORT_BOOT,
} from '@jbenet/feedback-react';

declare global {
  interface Window { I2F_FEEDBACK?: { page?: 'issues' | 'issue'; id?: string } }
}

const base = '/_i2f/feedback';
const issueHref = (id: string) => `${base}/issues/${encodeURIComponent(id)}`;
const config = {
  base,
  issueHref,
  storagePrefix: 'ipfs2fil-feedback',
  destinationNote: 'Saved on this machine, under ~/.ipfs2fil/feedback.',
};

new Function(VIEWPORT_BOOT)();

// The kit's pages link to /issues; here they live under base.
const Link = ({ href, className, children }: { href: string; className?: string; children?: ReactNode }) => (
  <a href={href.startsWith('/issues') ? base + href : href} className={className}>{children}</a>
);

// The automatic screenshot draws <noscript> text as if it were shown.
document.querySelectorAll('noscript').forEach((n) => n.remove());

const mount = document.createElement('div');
mount.id = 'i2f-feedback';
mount.className = 'nocapture';
document.body.append(mount);
const where = window.I2F_FEEDBACK ?? {};

createRoot(mount).render(
  <FeedbackProvider {...config}>
    {where.page === 'issues' && <div className="i2f-page"><IssuesPage heading="Explorer feedback" Link={Link} /></div>}
    {where.page === 'issue' && <div className="i2f-page"><IssuePage id={where.id ?? ''} Link={Link} editable /></div>}
    {/* The launcher (button and Alt+F) and where reports stand, kept out of the automatic screenshot. */}
    <span className="i2f-fbk-status"><FeedbackStatus variant="bar" /></span>
    <FeedbackButton variant="floating" />
  </FeedbackProvider>,
);
