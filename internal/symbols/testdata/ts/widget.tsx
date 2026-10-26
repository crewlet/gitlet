import React from "react";
import { Button } from "./button";
import * as icons from "./icons";

interface Props { title: string; onClose?: () => void }

export default function Widget({ title, onClose }: Props): JSX.Element {
  const [open, setOpen] = React.useState(false);
  return (
    <Panel title={title}>
      <icons.Close onClick={onClose} />
      <Button label="ok" />
      <div className="x">{format(title)}</div>
    </Panel>
  );
}

export const Panel = ({ title, children }: { title: string; children?: React.ReactNode }) => (
  <section>{children}</section>
);
