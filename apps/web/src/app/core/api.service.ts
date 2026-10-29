import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Appointment, LogEntry, Session } from './models';

export interface NewSession {
  code: string;
  provider: string;
  specialty: string;
  location: string;
  capacitySlots: number;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  sessions = () => firstValueFrom(this.http.get<Session[]>('/api/sessions'));
  openSession = (body: NewSession) =>
    firstValueFrom(this.http.post<Session>('/api/sessions', body));

  appointments = (sessionCode: string) =>
    firstValueFrom(this.http.get<Appointment[]>('/api/appointments', { params: { sessionCode } }));
  book = (body: { sessionCode: string; reference: string; slots: number }) =>
    firstValueFrom(this.http.post<Appointment>('/api/appointments', body));
  cancel = (id: string) =>
    firstValueFrom(this.http.post<Appointment>(`/api/appointments/${id}/cancellation`, {}));
  events = (id: string) =>
    firstValueFrom(this.http.get<LogEntry[]>(`/api/appointments/${id}/events`));
}
