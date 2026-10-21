import { useState, type ReactNode } from "react";
import { Info, AlertTriangle, CheckCircle2, XCircle } from "lucide-react";
import { PageTitle } from "@/components/common";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "@/components/ui/accordion";
import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogTitle, DialogTrigger, SheetContent } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input, NativeSelect, Textarea } from "@/components/ui/input";
import { Progress } from "@/components/ui/progress";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { SegmentedControl } from "@/components/ui/segmented-control";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { toast } from "@/components/ui/toaster";
import { Tooltip } from "@/components/ui/tooltip";

// The style guide: every token and component, as screens should use
// them (docs/plans/design-system.md). Not in the menus; open /app/design,
// and use the theme button for dark.

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section aria-label={title} className="flex flex-col gap-3">
      <h2 className="label">{title}</h2>
      <Card className="flex flex-col gap-4 p-4 md:p-5">{children}</Card>
    </section>
  );
}

const colors = [
  ["background", "bg-background", "The page"],
  ["surface", "bg-surface", "Cards, panels, inputs"],
  ["foreground", "bg-foreground", "Text"],
  ["muted-foreground", "bg-muted-foreground", "Secondary text"],
  ["border", "bg-border", "Hairlines"],
  ["muted", "bg-muted", "Quiet fills, hover"],
  ["primary", "bg-primary", "Work moving, links"],
  ["attention", "bg-attention", "Needs you"],
  ["success", "bg-success", "Went live, passed"],
  ["destructive", "bg-destructive", "Broken, failed"],
  ["ink", "bg-ink", "A decision waiting for you"],
] as const;

const sizes = [
  ["display", "text-display", "Page titles on Home"],
  ["title", "text-title", "Page titles"],
  ["title-sm", "text-title-sm", "Card titles, a decision"],
  ["body", "text-body", "Reading text"],
  ["body-sm", "text-body-sm", "Lists, controls"],
  ["meta", "text-meta", "Labels, details"],
  ["caption", "text-caption", "Badges, tiny notes"],
] as const;

export function DesignScreen() {
  const [host, setHost] = useState("github");
  const [on, setOn] = useState(true);
  return (
    <div className="mx-auto flex max-w-3xl min-w-0 flex-col gap-8" data-testid="design">
      <div>
        <PageTitle>Design</PageTitle>
        <p className="-mt-2 text-body-sm text-text-2">
          Factory's tokens and components. Build screens from these: the one type scale, the two radii, and the colors by their role.
        </p>
      </div>

      <Section title="Color">
        <ul className="grid grid-cols-2 gap-3 sm:grid-cols-3">
          {colors.map(([name, cls, use]) => (
            <li key={name} className="flex items-center gap-3">
              <span className={`size-10 shrink-0 rounded-control border ${cls}`} aria-hidden />
              <span className="flex min-w-0 flex-col">
                <span className="font-mono text-meta">{name}</span>
                <span className="text-caption text-muted-foreground">{use}</span>
              </span>
            </li>
          ))}
        </ul>
      </Section>

      <Section title="Type">
        <ul className="flex flex-col gap-3">
          {sizes.map(([name, cls, use]) => (
            <li key={name} className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
              <span className={`${cls} font-medium`}>Your OK on the Download CSV button</span>
              <span className="font-mono text-caption text-muted-foreground">
                {name} · {use}
              </span>
            </li>
          ))}
          <li className="font-mono text-body-sm">acme/web#41 · +84 −12 · $12.40 · Geist Mono for repos, numbers, times, money</li>
        </ul>
      </Section>

      <Section title="Buttons">
        <div className="flex flex-wrap gap-2">
          <Button>Look at it</Button>
          <Button variant="primary">Primary</Button>
          <Button variant="outline">Later</Button>
          <Button variant="ghost">Ghost</Button>
          <Button variant="destructive">Remove</Button>
          <Button variant="link">A link</Button>
          <Button disabled>Disabled</Button>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button size="lg">Large</Button>
          <Button>Default</Button>
          <Button size="sm">Small</Button>
        </div>
        <div className="flex flex-col gap-3 rounded-card bg-ink p-4 text-ink-foreground">
          <span className="text-meta font-semibold text-ink-attention">Needs you · acme/web#41</span>
          <span className="text-title-sm font-semibold">On an ink card, the main action is the light one</span>
          <div className="flex flex-wrap gap-2">
            <Button variant="inverse">Look at it</Button>
            <Button variant="inverse-outline">Merge</Button>
          </div>
        </div>
      </Section>

      <Section title="Badges">
        <div className="flex flex-wrap gap-2">
          <Badge>Working</Badge>
          <Badge variant="attention">Needs you</Badge>
          <Badge variant="success">Live</Badge>
          <Badge variant="destructive">Failed</Badge>
          <Badge variant="secondary">Optional</Badge>
          <Badge variant="outline">Outline</Badge>
        </div>
      </Section>

      <Section title="Forms">
        <Field id="design-repo" label="Repository" help="owner/name, or paste the repo's link.">
          <Input id="design-repo" placeholder="octocat/hello-world" />
        </Field>
        <Field id="design-token" label="GitHub token" error="That's a GitLab token.">
          <Input id="design-token" type="password" defaultValue="glpat-…" />
        </Field>
        <Field id="design-note" label="Note for the factory">
          <Textarea id="design-note" placeholder="What should change, and why" />
        </Field>
        <Field id="design-branch" label="Base branch">
          <NativeSelect id="design-branch" defaultValue="main">
            <option>main</option>
            <option>develop</option>
          </NativeSelect>
        </Field>
        <div className="flex flex-col gap-3">
          <span className="text-body-sm font-medium">Where is it hosted?</span>
          <SegmentedControl
            label="Where is it hosted?"
            value={host}
            onValueChange={setHost}
            options={[
              { value: "github", label: "GitHub" },
              { value: "gitlab", label: "GitLab" },
            ]}
          />
        </div>
        <label className="flex items-center gap-3 text-body-sm">
          <Checkbox defaultChecked /> All weekend
        </label>
        <label className="flex items-center justify-between gap-3 text-body-sm">
          Quiet hours
          <Switch checked={on} onCheckedChange={setOn} />
        </label>
        <RadioGroup defaultValue="legacy" aria-label="Kind of token">
          <label className="flex items-center gap-3 text-body-sm">
            <RadioGroupItem value="legacy" /> Legacy, the quick one
          </label>
          <label className="flex items-center gap-3 text-body-sm">
            <RadioGroupItem value="fine" /> Fine-grained
          </label>
        </RadioGroup>
      </Section>

      <Section title="Alerts">
        <Alert variant="info">
          <Info aria-hidden />
          <span>The factory restarts once running jobs finish.</span>
        </Alert>
        <Alert variant="attention">
          <AlertTriangle aria-hidden />
          <span>Auto-merge is paused on acme/api after 2 reverts this week.</span>
        </Alert>
        <Alert variant="success">
          <CheckCircle2 aria-hidden />
          <span>Since you looked: 2 went live.</span>
        </Alert>
        <Alert variant="danger">
          <XCircle aria-hidden />
          <span>main is red on acme/web: test fails.</span>
        </Alert>
      </Section>

      <Section title="Progress">
        <Progress label="Spent today" value={12.4} max={25} />
        <Progress label="Setup" value={7} max={7} tone="success" />
        <Progress label="Budget nearly gone" value={23} max={25} tone="attention" />
      </Section>

      <Section title="Tabs and accordion">
        <Tabs defaultValue="web">
          <TabsList>
            <TabsTrigger value="web">acme/web</TabsTrigger>
            <TabsTrigger value="api">acme/api</TabsTrigger>
          </TabsList>
          <TabsContent value="web" className="text-body-sm">Trust level 2: small, low-risk changes merge themselves.</TabsContent>
          <TabsContent value="api" className="text-body-sm">Trust level 1: every change waits for you.</TabsContent>
        </Tabs>
        <Accordion type="single" collapsible>
          <AccordionItem value="a">
            <AccordionTrigger>Other ways to connect</AccordionTrigger>
            <AccordionContent>An app you already made, or a personal access token.</AccordionContent>
          </AccordionItem>
          <AccordionItem value="b">
            <AccordionTrigger>3 checks passed</AccordionTrigger>
            <AccordionContent>git 2.50, Docker works, disk has room.</AccordionContent>
          </AccordionItem>
        </Accordion>
      </Section>

      <Section title="Overlays">
        <div className="flex flex-wrap gap-2">
          <Dialog>
            <DialogTrigger asChild>
              <Button variant="outline">Open a dialog</Button>
            </DialogTrigger>
            <DialogContent>
              <DialogTitle>Disconnect the app?</DialogTitle>
              <DialogDescription>It stays on GitHub, and its key stays on this machine.</DialogDescription>
              <div className="flex justify-end gap-2">
                <Button variant="outline">Keep it</Button>
                <Button variant="destructive">Disconnect</Button>
              </div>
            </DialogContent>
          </Dialog>
          <Dialog>
            <DialogTrigger asChild>
              <Button variant="outline">Open a sheet</Button>
            </DialogTrigger>
            <SheetContent>
              <DialogTitle>Ask the factory</DialogTitle>
              <DialogDescription>Sheets come up from the bottom on a phone.</DialogDescription>
            </SheetContent>
          </Dialog>
          <Tooltip content="Small, low-risk changes merge themselves">
            <Button variant="outline">Hover for a tooltip</Button>
          </Tooltip>
          <Button variant="outline" onClick={() => toast("Saved the token. The factory can reach acme/web.")}>
            Show a toast
          </Button>
        </div>
      </Section>
    </div>
  );
}
