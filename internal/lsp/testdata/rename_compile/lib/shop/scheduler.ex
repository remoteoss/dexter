defmodule Shop.Scheduler do
  @moduledoc false
  alias Shop.{Job, Mailer}

  def go(arg) do
    Job.perform(arg)
    Job.cancel(arg)
    Enum.each([arg], &Mailer.perform/1)
    Shop.Worker.run_all([Job, Mailer], arg)
    Job.go(5)
    5 |> Job.go()
    Enum.each([arg], &Job.go/1)
    Job.go(1, [])
    Job.go(1, 2, 3)
  end
end
