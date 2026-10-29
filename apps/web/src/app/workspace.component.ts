import { Component, DestroyRef, OnInit, computed, inject, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { ApiService } from './core/api.service';
import { AuthService } from './core/auth.service';
import { Appointment, AppointmentStatus, LogEntry, Session } from './core/models';

const BADGES: Record<AppointmentStatus, string> = {
  Requested: 'bg-slate-100 text-slate-700 ring-slate-300',
  Held: 'bg-amber-50 text-amber-800 ring-amber-300',
  Confirmed: 'bg-emerald-50 text-emerald-800 ring-emerald-300',
  Cancelled: 'bg-sky-50 text-sky-800 ring-sky-300',
  Rejected: 'bg-rose-50 text-rose-800 ring-rose-300',
};

@Component({
  selector: 'app-workspace',
  imports: [DatePipe],
  templateUrl: './workspace.component.html',
})
export class WorkspaceComponent implements OnInit {
  private readonly api = inject(ApiService);
  readonly auth = inject(AuthService);
  private readonly destroyRef = inject(DestroyRef);

  readonly sessions = signal<Session[]>([]);
  readonly selectedCode = signal('');
  readonly appointments = signal<Appointment[]>([]);
  readonly log = signal<LogEntry[]>([]);
  readonly logFor = signal<string | null>(null);
  readonly error = signal('');

  readonly code = signal('');
  readonly provider = signal('Dr. Silva');
  readonly specialty = signal('Cardiology');
  readonly location = signal('Clinic A');
  readonly capacity = signal('12');

  readonly reference = signal('');
  readonly slots = signal('1');

  readonly selected = computed(() => this.sessions().find((s) => s.code === this.selectedCode()));

  ngOnInit(): void {
    void this.refresh();
    const timer = setInterval(() => void this.refresh(), 1000);
    this.destroyRef.onDestroy(() => clearInterval(timer));
  }

  badge(status: AppointmentStatus): string {
    return BADGES[status];
  }

  money(cents: number): string {
    return (cents / 100).toLocaleString('en-US', { style: 'currency', currency: 'USD' });
  }

  async refresh(): Promise<void> {
    try {
      const sessions = await this.api.sessions();
      this.sessions.set(sessions);
      if (!this.selectedCode() && sessions.length > 0) this.selectedCode.set(sessions[0].code);
      if (this.selectedCode())
        this.appointments.set(await this.api.appointments(this.selectedCode()));
      const id = this.logFor();
      if (id && this.auth.isStaff()) this.log.set(await this.api.events(id));
    } catch (e: unknown) {
      // A 401 means the session is gone; the interceptor has already signed the user out.
      if ((e as { status?: number }).status !== 401) this.error.set('The API is not reachable.');
      return;
    }
    if (this.error() === 'The API is not reachable.') this.error.set('');
  }

  async select(code: string): Promise<void> {
    this.selectedCode.set(code);
    this.logFor.set(null);
    this.log.set([]);
    await this.refresh();
  }

  async openSession(): Promise<void> {
    await this.run(async () => {
      const code = this.code();
      const session = await this.api.openSession({
        code,
        provider: this.provider(),
        specialty: this.specialty(),
        location: this.location(),
        capacitySlots: Number(this.capacity()),
      });
      if (this.code() === code) this.code.set('');
      this.selectedCode.set(session.code);
    });
  }

  async book(): Promise<void> {
    await this.run(async () => {
      const reference = this.reference();
      await this.api.book({
        sessionCode: this.selectedCode(),
        reference,
        slots: Number(this.slots()),
      });
      if (this.reference() === reference) this.reference.set('');
    });
  }

  async cancel(appointment: Appointment): Promise<void> {
    await this.run(() => this.api.cancel(appointment.id));
  }

  async showLog(appointment: Appointment): Promise<void> {
    this.logFor.set(appointment.id);
    this.log.set([]);
    await this.refresh();
  }

  canCancel(appointment: Appointment): boolean {
    return appointment.status === 'Held' || appointment.status === 'Confirmed';
  }

  private async run(action: () => Promise<unknown>): Promise<void> {
    this.error.set('');
    try {
      await action();
    } catch (e: unknown) {
      const body = (e as { error?: { title?: string } }).error;
      this.error.set(body?.title ?? 'The request failed.');
    }
    await this.refresh();
  }

  field(signalRef: { set(value: string): void }, event: Event): void {
    signalRef.set((event.target as HTMLInputElement).value);
  }
}
