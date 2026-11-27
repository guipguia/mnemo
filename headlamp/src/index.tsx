import {
  DetailsViewSectionProps,
  registerDetailsViewSection,
  registerPluginSettings,
} from '@kinvolk/headlamp-plugin/lib';
import { DateLabel, SectionBox, SimpleTable } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { getCluster } from '@kinvolk/headlamp-plugin/lib/Utils';
import { Link, Typography } from '@mui/material';
import { useEffect, useState } from 'react';
import { findHub, history, mnemoCluster, PLUGIN, Ref, store, uiLink, Version } from './mnemo';
import { Settings } from './settings';

type State =
  | { status: 'loading' }
  | { status: 'none'; why: string }
  | { status: 'ok'; ref: Ref; versions: Version[] };

const useConf = store.useConfig();

// Kinds mnemo never versions: Secrets are refused, Events archived apart.
const unrecorded = new Set(['Secret', 'Event']);

function History({ resource }: { resource: DetailsViewSectionProps['resource'] }) {
  const [state, setState] = useState<State>({ status: 'loading' });
  const conf = useConf();
  const json = resource.jsonData;
  const apiVersion: string = json.apiVersion ?? '';
  const group = apiVersion.includes('/') ? apiVersion.split('/')[0] : '';
  const namespace = json.metadata?.namespace ?? '';
  const name = json.metadata?.name ?? '';
  const kind: string = json.kind ?? '';
  const viewed = getCluster();

  useEffect(() => {
    let live = true;
    setState({ status: 'loading' }); // not the previous object's rows meanwhile
    (async () => {
      const hub = await findHub(conf ?? {}, viewed);
      if (!hub) {
        return {
          status: 'none',
          why: 'No mnemo hub found: set where it runs in the plugin settings.',
        } as State;
      }
      const cluster = await mnemoCluster(hub, viewed);
      if (!cluster) {
        return { status: 'none', why: 'mnemo does not record this cluster.' } as State;
      }
      const ref = { cluster, group, kind, namespace, name };
      return { status: 'ok', ref, versions: (await history(hub, ref)) ?? [] } as State;
    })()
      .catch((err: { status?: number; message?: string }) => {
        const why =
          err?.status === 401
            ? 'The mnemo hub asks for a login: the plugin needs a hub with --auth none, reached through the service proxy.'
            : err?.status === 403
            ? 'You may not reach the mnemo hub: it takes get on services/proxy for its Service, and list on services to find it (or set it in the plugin settings).'
            : `mnemo: ${err?.message ?? err}`;
        return { status: 'none', why } as State;
      })
      .then(s => live && setState(s));
    return () => {
      live = false;
    };
  }, [viewed, conf?.cluster, conf?.namespace, conf?.service, group, kind, namespace, name]);

  const link = conf?.uiURL && state.status === 'ok' ? uiLink(conf.uiURL, state.ref) : undefined;

  return (
    <SectionBox title="History (mnemo)">
      {state.status === 'loading' && <Typography>Loading…</Typography>}
      {state.status === 'none' && <Typography color="text.secondary">{state.why}</Typography>}
      {state.status === 'ok' && (
        <>
          {state.versions.length === 0 ? (
            <Typography color="text.secondary">
              No recorded versions: mnemo may not watch this kind.
            </Typography>
          ) : (
            <SimpleTable
              data={state.versions}
              columns={[
                { label: 'When', getter: (v: Version) => <DateLabel date={v.time} /> },
                { label: 'Change', getter: (v: Version) => v.type },
                { label: 'Who', getter: (v: Version) => v.author || 'unknown' },
                {
                  label: 'How',
                  getter: (v: Version) => (v.drift ? `${v.source} (drift)` : v.source),
                },
                {
                  label: 'Fields',
                  getter: (v: Version) =>
                    (v.paths ?? []).slice(0, 3).join(', ') +
                    ((v.paths?.length ?? 0) > 3 ? ` +${v.paths!.length - 3}` : ''),
                },
              ]}
            />
          )}
          {link && (
            <Link href={link} target="_blank" rel="noopener noreferrer">
              Open in mnemo: diffs and the object at any time
            </Link>
          )}
        </>
      )}
    </SectionBox>
  );
}

registerDetailsViewSection(({ resource }: DetailsViewSectionProps) =>
  resource?.jsonData?.kind && !unrecorded.has(resource.jsonData.kind) ? (
    <History resource={resource} />
  ) : null
);

registerPluginSettings(PLUGIN, Settings, true);
