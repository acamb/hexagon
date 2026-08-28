// How each status reads in the UI. Provisioning steps get a sentence, because
// "creating" on its own tells the reader nothing about what is happening.
import type { SessionStatus } from './api'

const labels: Record<SessionStatus, string> = {
  creating: 'Preparing the workspace…',
  cloning: 'Cloning the repository…',
  starting: 'Starting the container…',
  running: 'Running',
  stopped: 'Stopped',
  failed: 'Failed',
  gone: 'Container gone',
}

export function sessionLabel(status: SessionStatus): string {
  return labels[status] ?? status
}

// A session in one of these is still being set up, so the page keeps polling.
export function isProvisioning(status: SessionStatus): boolean {
  return status === 'creating' || status === 'cloning' || status === 'starting'
}
