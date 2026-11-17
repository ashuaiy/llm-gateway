"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, currentRole } from "@/lib/api";
import type { BillingJob, BillingResolution } from "@/lib/types";
import { PageHeader } from "@/components/ui/page-header";
import { Panel, PanelBody } from "@/components/ui/panel";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { toast } from "sonner";

const usd = (units: number) => `$${(units / 1e9).toFixed(8)}`;
export default function BillingPage() {
  const [role] = useState(() => currentRole());
  const allowed = role === "super_admin" || role === "tenant_admin";
  const [offset, setOffset] = useState(0);
  const [lookup, setLookup] = useState("");
  const [selected, setSelected] = useState<BillingJob | null>(null);
  const [action, setAction] = useState<"settle" | "release">("settle");
  const [input, setInput] = useState("0");
  const [output, setOutput] = useState("0");
  const [reason, setReason] = useState("");
  const [evidence, setEvidence] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [attempt, setAttempt] = useState<{ fingerprint: string; id: string } | null>(null);
  const query = useQuery({ queryKey: ["billing-reservations", offset], queryFn: () => api.getBillingReservations(offset), enabled: allowed });
  function select(job: BillingJob) {
    setSelected(job); setInput("0"); setOutput("0"); setReason(""); setEvidence(""); setConfirmed(false); setAttempt(null);
  }
  async function find() {
    setBusy(true);
    try { select((await api.getBillingReservation(lookup.trim())).reservation); }
    catch (err) { toast.error(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  }
  const actual = selected && action === "settle" ? Number(input) * selected.reservation.input_rate + Number(output) * selected.reservation.output_rate : 0;
  async function resolve(e: React.FormEvent) {
    e.preventDefault();
    if (!selected || !confirmed) return;
    const fields = { action, input_tokens: action === "release" ? 0 : Number(input), output_tokens: action === "release" ? 0 : Number(output), reason: action === "release" ? "confirmed_not_billed" : reason.trim(), evidence: evidence.trim() };
    const fingerprint = JSON.stringify([selected.reservation.id, fields]);
    const id = attempt?.fingerprint === fingerprint ? attempt.id : crypto.randomUUID();
    setAttempt({ fingerprint, id });
    const payload: BillingResolution = { ...fields, resolution_id: id };
    setBusy(true);
    try {
      await api.reconcileReservation(selected.reservation.id, payload);
      toast.success("Resolution accepted; durable billing follows asynchronously");
      setSelected((await api.getBillingReservation(selected.reservation.id)).reservation);
      await query.refetch();
    } catch (err) { toast.error(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  }
  if (!allowed) return <p className="p-6">Administrator access required.</p>;
  return <div>
    <PageHeader code="billing" title="Billing review" description="Review unresolved usage against provider evidence before settling or releasing held funds." actions={<Button onClick={() => query.refetch()} disabled={busy}>Refresh</Button>} />
    <div className="mb-5 flex flex-wrap gap-2">
      <Input aria-label="Reservation ID" className="max-w-md" placeholder="Find a reservation by ID, including settled bills" value={lookup} onChange={e => setLookup(e.target.value)} />
      <Button onClick={find} disabled={busy || !lookup.trim()}>Find</Button>
    </div>
    {query.isError && <p role="alert" className="mb-4 text-destructive">{query.error.message}. Billing review requires reservation mode.</p>}
    <div className="grid gap-5 xl:grid-cols-2">
      <Panel><PanelBody>
        <h2 className="mb-4 font-display text-lg">Unresolved reservations</h2>
        {query.isLoading && <p>Loading…</p>}
        {query.data?.reservations.length === 0 && <p className="text-muted-foreground">No visible reservations in this scan page. Later pages may contain more.</p>}
        <div className="space-y-2">{query.data?.reservations.map(job => <button disabled={busy} key={job.reservation.id} onClick={() => select(job)} className="w-full rounded-sm border border-border p-3 text-left hover:bg-secondary">
          <span className="block break-all font-mono text-xs">{job.reservation.id}</span>
          <span className="text-sm">Key {job.reservation.key_id} · {job.reservation.model} · {job.state} · held {usd(job.reservation.amount)}</span>
          <span className="block text-xs text-muted-foreground">{new Date(job.created_millis).toLocaleString()}</span>
        </button>)}</div>
        <div className="mt-4 flex items-center gap-3">
          <Button variant="outline" disabled={offset === 0 || busy} onClick={() => setOffset(Math.max(0, offset - 50))}>Previous</Button>
          <span className="text-xs">Scan offset {offset}</span>
          <Button variant="outline" disabled={!query.data || busy} onClick={() => setOffset(query.data!.next_offset)}>Next</Button>
        </div>
      </PanelBody></Panel>
      <Panel><PanelBody>
        <h2 className="mb-4 font-display text-lg">Resolution</h2>
        {!selected ? <p className="text-muted-foreground">Select a reservation to inspect its frozen prices and usage.</p> : <>
          <p className="break-all font-mono text-xs">{selected.reservation.id}</p>
          <p className="my-3 text-sm">{selected.state} · held {usd(selected.reservation.amount)} · actual {usd(selected.settlement.actual)}</p>
          {selected.unresolved_reason && <p>{selected.unresolved_reason}</p>}
          {selected.reported_settlement && <div className="my-3 rounded-sm border border-border p-3 text-sm">
            <p>Provider-reported usage awaiting review ({selected.reported_settlement.source})</p>
            <p>Input {selected.reported_settlement.input_tokens} · Output {selected.reported_settlement.output_tokens} · Charge {usd(selected.reported_settlement.actual)}</p>
          </div>}
          {selected.resolution && <pre className="my-3 whitespace-pre-wrap break-all text-xs">{JSON.stringify(selected.resolution, null, 2)}</pre>}
          {!["settled", "released"].includes(selected.state) && <form onSubmit={resolve} className="space-y-4">
            <fieldset disabled={busy} className="space-y-4">
              <Label htmlFor="resolution-action">Action</Label>
              <select id="resolution-action" value={action} onChange={e => { setAction(e.target.value as "settle" | "release"); setConfirmed(false); }} className="w-full rounded-sm border border-border bg-background p-2">
                <option value="settle">Settle verified provider usage</option><option value="release">Release — confirmed not billed</option>
              </select>
              {action === "settle" && <>
                <Label htmlFor="billing-input">Input tokens · {usd(selected.reservation.input_rate)} / token</Label>
                <Input id="billing-input" type="number" min="0" max="1000000000" step="1" required value={input} onChange={e => { setInput(e.target.value); setConfirmed(false); }} />
                <Label htmlFor="billing-output">Output tokens · {usd(selected.reservation.output_rate)} / token</Label>
                <Input id="billing-output" type="number" min="0" max="1000000000" step="1" required value={output} onChange={e => { setOutput(e.target.value); setConfirmed(false); }} />
                <Label htmlFor="billing-reason">Reason</Label><Input id="billing-reason" required maxLength={1000} value={reason} onChange={e => setReason(e.target.value)} />
              </>}
              <Label htmlFor="billing-evidence">Evidence reference (provider request ID / billing export; no personal data or secrets)</Label>
              <Input id="billing-evidence" required maxLength={2000} value={evidence} onChange={e => setEvidence(e.target.value)} />
              <p className="text-sm">Charge {usd(actual)} · {actual <= selected.reservation.amount ? "release" : "additional charge"} {usd(Math.abs(selected.reservation.amount - actual))}</p>
              <label className="flex items-start gap-2 text-sm"><input type="checkbox" required checked={confirmed} onChange={e => setConfirmed(e.target.checked)} />I verified the evidence and amounts{action === "release" ? ", including confirmation that the provider did not bill this request" : ""}.</label>
              <Button type="submit" disabled={!confirmed || !evidence.trim() || (action === "settle" && !reason.trim())}>Record resolution</Button>
            </fieldset>
          </form>}
        </>}
      </PanelBody></Panel>
    </div>
  </div>;
}
