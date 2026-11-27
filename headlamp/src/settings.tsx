import { NameValueTable } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import TextField from '@mui/material/TextField';
import { Conf } from './mnemo';

type Props = { data?: Conf; onDataChange?: (data: Conf) => void };

const fields: { key: keyof Conf; name: string; help: string }[] = [
  {
    key: 'cluster',
    name: 'Hub cluster',
    help: 'The Headlamp cluster the mnemo hub runs in. Empty: the cluster being viewed.',
  },
  {
    key: 'namespace',
    name: 'Hub namespace',
    help: 'With the Service name; empty: found by label.',
  },
  {
    key: 'service',
    name: 'Hub Service',
    help: 'The hub Service, e.g. mnemo; empty: found by label.',
  },
  {
    key: 'uiURL',
    name: 'mnemo UI URL',
    help: 'e.g. https://mnemo.example.com, for "Open in mnemo" links. Empty: no links.',
  },
];

export function Settings({ data, onDataChange }: Props) {
  return (
    <NameValueTable
      rows={fields.map(f => ({
        name: f.name,
        value: (
          <TextField
            fullWidth
            variant="standard"
            helperText={f.help}
            defaultValue={data?.[f.key] ?? ''}
            onChange={e => onDataChange?.({ ...data, [f.key]: e.target.value.trim() })}
          />
        ),
      }))}
    />
  );
}
